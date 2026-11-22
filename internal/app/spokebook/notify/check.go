package notify

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spokebook/spokebook/internal/app/spokebook/catalog"
	"github.com/spokebook/spokebook/internal/app/spokebook/components"
	"github.com/spokebook/spokebook/internal/app/spokebook/due"
	"github.com/spokebook/spokebook/internal/app/spokebook/i18n"
	"github.com/spokebook/spokebook/internal/app/spokebook/odometer"
	"github.com/spokebook/spokebook/internal/app/spokebook/rides/garmin"
	"github.com/spokebook/spokebook/internal/app/spokebook/store"
)

const (
	// every is the time between two checks of an owner.
	every = 24 * time.Hour
	// checkEvery is how often the runner looks for owners due for a check.
	checkEvery = 10 * time.Minute
)

// sendTimeout is how long a check may take sending, so a slow receiver
// can't stall it.
var sendTimeout = 2 * time.Minute

// States of conditions; maintenance and wear use due.State.String(),
// rides without bike their number.
const (
	stateOverdue = "overdue"
	// stateOpen: an open maintenance.
	stateOpen = "open"
	// stateError: Garmin Connect needs a new API key.
	stateError = "error"
)

// keyUnassigned is the key of the rides without bike.
const keyUnassigned = "unassigned"

// rank orders the states of a condition; it is notified again when its
// rank rises. The rank of the rides without bike is their number.
func rank(key, state string) int {
	if key == keyUnassigned {
		n, _ := strconv.Atoi(state)
		return n
	}
	if state == stateOverdue {
		return 2
	}
	return 1
}

// condition is something the owner may be notified about. Key identifies
// it across checks, e.g. "level:<bike>:minor".
type condition struct {
	Key   string
	State string
	// BikeID is set for conditions of a bike; they are sent together.
	BikeID   string
	BikeName string
	// Line describes the condition in a message of a bike; Msg is the
	// message of a condition without bike.
	Line string
	Msg  Message
	// Covered means a higher level of the bike is pending, at least as
	// urgent, and includes this one: its state is stored, but it is not
	// notified itself.
	Covered bool
}

// Run checks every owner with an enabled channel once a day, until ctx is
// done. The time of the last check is stored, so restarts don't add
// checks.
func (s *Service) Run(ctx context.Context) {
	if !s.Enabled() {
		return
	}
	tick := time.NewTicker(checkEvery)
	defer tick.Stop()
	s.runDue(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.runDue(ctx)
		}
	}
}

// RunDue checks the owners whose last check is a day ago or who were
// never checked.
func (s *Service) RunDue(ctx context.Context) { s.runDue(ctx) }

func (s *Service) runDue(ctx context.Context) {
	owners, err := s.st.Notifications().ListNotificationOwners(ctx)
	if err != nil {
		s.log.Error("notify: list owners", "err", err)
		return
	}
	now := s.now()
	for _, o := range owners {
		if o.LastRunAt != nil && now.Sub(*o.LastRunAt) < every {
			continue
		}
		if err := s.Check(ctx, o.OwnerID); err != nil && ctx.Err() == nil {
			s.log.Error("notify: check", "user", o.OwnerID, "err", err)
		}
	}
}

// Check notifies the owner of the conditions of the enabled events that
// are new or got worse since the last check, on every enabled channel,
// and records the check. Without APP_SECRET_KEY or enabled channel it
// does nothing. A condition whose message no channel took is notified
// again next time; one that is gone is forgotten, so it is notified again
// when it comes back. Sending stops after sendTimeout.
func (s *Service) Check(ctx context.Context, ownerID string) error {
	if !s.Enabled() {
		return nil
	}
	defer LockOwner(ownerID)()
	list, err := s.st.Notifications().ListNotificationChannels(ctx, ownerID)
	if err != nil {
		return err
	}
	channels := slices.DeleteFunc(list, func(c store.NotificationChannel) bool { return !c.Enabled })
	if len(channels) == 0 {
		return nil
	}
	now := s.now().UTC()
	var settings store.NotificationSettings
	err = s.st.WithTx(ctx, func(tx store.Stores) error {
		ns := tx.Notifications()
		settings, err = ns.GetNotificationSettings(ctx, ownerID)
		if errors.Is(err, store.ErrNotFound) {
			settings = Defaults(ownerID)
			err = ns.SaveNotificationSettings(ctx, settings)
		}
		if err != nil {
			return err
		}
		return ns.SetNotificationLastRun(ctx, ownerID, now)
	})
	if err != nil {
		return err
	}
	l := s.localizer(settings.Language)
	conds, err := s.conditions(ctx, ownerID, settings, l, now)
	if err != nil {
		return err
	}
	states, err := s.st.Notifications().ListNotificationStates(ctx, ownerID)
	if err != nil {
		return err
	}
	old := make(map[string]string, len(states))
	for _, st := range states {
		old[st.Key] = st.State
	}
	// fresh are the conditions to notify; quiet the changed ones not to
	// notify (got better, or covered by a higher level).
	var fresh, quiet []condition
	current := make(map[string]bool, len(conds))
	for _, c := range conds {
		current[c.Key] = true
		prev, ok := old[c.Key]
		switch {
		case !c.Covered && (!ok || rank(c.Key, c.State) > rank(c.Key, prev)):
			fresh = append(fresh, c)
		case !ok || c.State != prev:
			quiet = append(quiet, c)
		}
	}
	msgs, parts := s.messages(fresh, l)
	delivered := s.deliver(ctx, channels, msgs)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return s.st.WithTx(ctx, func(tx store.Stores) error {
		ns := tx.Notifications()
		// The last enabled channel may have gone during the run, which
		// forgot the states; they stay forgotten.
		list, err := ns.ListNotificationChannels(ctx, ownerID)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(list, func(c store.NotificationChannel) bool { return c.Enabled }) {
			return nil
		}
		for _, st := range states {
			if !current[st.Key] {
				if err := ns.DeleteNotificationState(ctx, ownerID, st.Key); err != nil {
					return err
				}
			}
		}
		save := quiet
		for i, ok := range delivered {
			if ok {
				save = append(save, parts[i]...)
			}
		}
		for _, c := range save {
			if err := ns.SetNotificationState(ctx, ownerID, store.NotificationState{Key: c.Key, State: c.State, UpdatedAt: now}); err != nil {
				return err
			}
		}
		return nil
	})
}

// deliver sends the messages on every channel and records the result on
// each; it returns which messages at least one channel took. A channel
// stops at its first error; all sending stops after sendTimeout, and the
// channels not tried by then keep their last result.
func (s *Service) deliver(ctx context.Context, channels []store.NotificationChannel, msgs []Message) []bool {
	delivered := make([]bool, len(msgs))
	if len(msgs) == 0 {
		return delivered
	}
	sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	for _, c := range channels {
		if sendCtx.Err() != nil {
			return delivered
		}
		var err error
		sent := false
		kind, ok := s.Kind(c.Kind)
		var ch Channel
		cfg, oerr := s.open(c)
		switch {
		case !ok:
			err = ErrUnknownKind
		case oerr != nil:
			err = oerr
		default:
			ch, err = kind.Open(cfg)
		}
		for i := 0; err == nil && i < len(msgs); i++ {
			if err = ch.Send(sendCtx, msgs[i]); err == nil {
				delivered[i], sent = true, true
			}
		}
		if ctx.Err() != nil {
			return delivered
		}
		if rerr := s.record(ctx, c, err, sent); rerr != nil {
			s.log.Error("notify: record result", "user", c.OwnerID, "channel", c.ID, "err", rerr)
		}
	}
	return delivered
}

// messages groups the conditions to notify: one message per bike, then
// one per condition without bike. parts are the conditions of each
// message.
func (s *Service) messages(conds []condition, l *i18n.Localizer) ([]Message, [][]condition) {
	var msgs []Message
	var parts [][]condition
	byBike := map[string]int{}
	for _, c := range conds {
		if c.BikeID == "" {
			continue
		}
		i, ok := byBike[c.BikeID]
		if !ok {
			i = len(parts)
			byBike[c.BikeID] = i
			parts = append(parts, nil)
		}
		parts[i] = append(parts[i], c)
	}
	for _, p := range parts {
		m := Message{
			Title: l.T("notify.bike.title", p[0].BikeName), Priority: PriorityDefault, Tags: []string{"wrench"},
			Link: s.baseURL + "/bikes/" + p[0].BikeID,
		}
		lines := make([]string, len(p))
		for i, c := range p {
			lines[i] = "- " + c.Line
			if c.State == stateOverdue {
				m.Priority, m.Tags = PriorityHigh, []string{"warning"}
			}
		}
		m.Body = strings.Join(lines, "\n")
		msgs = append(msgs, m)
	}
	for _, c := range conds {
		if c.BikeID == "" {
			msgs = append(msgs, c.Msg)
			parts = append(parts, []condition{c})
		}
	}
	return msgs, parts
}

// conditions returns the current conditions of the owner's enabled
// events. Retired bikes and retired or removed components never count.
func (s *Service) conditions(ctx context.Context, ownerID string, ns store.NotificationSettings, l *i18n.Localizer, now time.Time) ([]condition, error) {
	on := func(e string) bool { return slices.Contains(ns.Events, e) }
	counts := func(st due.State) bool {
		return st == due.Overdue || (st == due.Soon && ns.Threshold != ThresholdOverdue)
	}
	all, err := s.st.Bikes().ListBikes(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	bikes := slices.DeleteFunc(all, func(b store.Bike) bool { return !b.Active() })
	var out []condition
	for _, b := range bikes {
		if on(EventMaintenance) {
			items, err := due.Load(ctx, s.st, ownerID, b, now)
			if err != nil {
				return nil, err
			}
			// The highest pending level includes the lower ones (as in
			// due.Next), so a lower one is only notified if it is more
			// urgent.
			top, topState := -1, ""
			for _, it := range items {
				if i := slices.Index(catalog.Levels, it.Level); it.Open == nil && counts(it.State) && it.Level != "" && i > top {
					top, topState = i, it.State.String()
				}
			}
			for _, it := range items {
				if it.Open != nil || !counts(it.State) {
					continue
				}
				c := condition{State: it.State.String(), BikeID: b.ID, BikeName: b.Name}
				if it.Level != "" {
					c.Key = "level:" + b.ID + ":" + it.Level
					c.Line = l.T("notify.item.level."+c.State, l.T("level."+it.Level))
					c.Covered = slices.Index(catalog.Levels, it.Level) < top && rank(c.Key, topState) >= rank(c.Key, c.State)
				} else {
					c.Key = "step:" + b.ID + ":" + it.Step.ID
					title, _ := catalog.BuiltinTexts(l, it.Step.BuiltinID, it.Step.Title, "")
					c.Line = l.T("notify.item.step."+c.State, title)
				}
				out = append(out, c)
			}
		}
	}
	if on(EventWear) {
		wear, err := s.wear(ctx, ownerID, bikes, counts, l, now)
		if err != nil {
			return nil, err
		}
		out = append(out, wear...)
	}
	if on(EventOpenMaintenance) {
		for _, b := range bikes {
			list, err := s.st.Maintenances().ListMaintenances(ctx, ownerID, b.ID)
			if err != nil {
				return nil, err
			}
			for _, m := range list {
				if m.Open() && now.Sub(m.CreatedAt) > time.Duration(ns.OpenDays)*24*time.Hour {
					out = append(out, condition{
						Key: "open:" + m.ID, State: stateOpen, BikeID: b.ID, BikeName: b.Name,
						Line: l.T("notify.item.open", l.Date(m.PerformedAt)),
					})
				}
			}
		}
	}
	if on(EventUnassigned) {
		rides, err := s.st.Rides().ListRides(ctx, ownerID)
		if err != nil {
			return nil, err
		}
		n := 0
		for _, r := range rides {
			if r.BikeID == "" && !r.Discarded {
				n++
			}
		}
		if n > 0 {
			out = append(out, condition{Key: keyUnassigned, State: strconv.Itoa(n), Msg: Message{
				Title: l.T("notify.unassigned.title"), Body: l.N("notify.unassigned.body", n),
				Priority: PriorityDefault, Tags: []string{"bike"}, Link: s.baseURL + "/rides?bike=none",
			}})
		}
	}
	if on(EventGarmin) {
		src, err := s.st.Rides().GetRideSource(ctx, ownerID, garmin.Source)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		if err == nil && (src.LastError == garmin.ErrorUnauthorized || src.LastError == garmin.ErrorSecret) {
			out = append(out, condition{Key: "garmin", State: stateError, Msg: Message{
				Title: l.T("notify.garmin.title"), Body: l.T("notify.garmin." + src.LastError),
				Priority: PriorityDefault, Tags: []string{"warning"}, Link: s.baseURL + "/rides/garmin",
			}})
		}
	}
	return out, nil
}

// wear returns the conditions of the components installed on the bikes.
func (s *Service) wear(ctx context.Context, ownerID string, bikes []store.Bike, counts func(due.State) bool, l *i18n.Localizer, now time.Time) ([]condition, error) {
	comps, err := s.st.Components().ListComponents(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	insts, err := s.st.Components().ListAllInstallations(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	latest, err := odometer.Latest(ctx, s.st, ownerID)
	if err != nil {
		return nil, err
	}
	types, err := s.st.Components().ListTypes(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	typeNames := make(map[string]string, len(types))
	for _, t := range types {
		typeNames[t.ID] = components.TypeName(l, t)
	}
	byComp := make(map[string][]store.Installation)
	for _, i := range insts {
		byComp[i.ComponentID] = append(byComp[i.ComponentID], i)
	}
	var out []condition
	for _, b := range bikes {
		for _, c := range comps {
			if !c.Active() || (c.MaxDistanceM == nil && c.ReplaceBy == nil) ||
				!slices.ContainsFunc(byComp[c.ID], func(i store.Installation) bool { return i.Open() && i.BikeID == b.ID }) {
				continue
			}
			w := components.WearOf(c, components.Mileage(byComp[c.ID], latest), now)
			st := due.OK
			switch {
			case w.Alert():
				st = due.Overdue
			case w.Percent >= due.SoonPercent:
				st = due.Soon
			}
			if !counts(st) {
				continue
			}
			name := typeNames[c.Type]
			if n := strings.TrimSpace(c.Manufacturer + " " + c.Model); n != "" {
				name += " (" + n + ")"
			}
			out = append(out, condition{
				Key: "component:" + c.ID, State: st.String(), BikeID: b.ID, BikeName: b.Name,
				Line: l.T("notify.item.wear."+st.String(), name),
			})
		}
	}
	return out, nil
}
