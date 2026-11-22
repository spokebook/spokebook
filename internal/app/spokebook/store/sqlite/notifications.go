package sqlite

import (
	"context"
	"strings"
	"time"

	"github.com/spokebook/spokebook/internal/app/spokebook/store"
	"github.com/spokebook/spokebook/internal/app/spokebook/store/sqlite/db"
)

type notificationStore struct{ q *db.Queries }

func toNotificationChannel(r db.NotificationChannel) (store.NotificationChannel, error) {
	out := store.NotificationChannel{
		ID: r.ID, OwnerID: r.OwnerID, Kind: r.Kind, Name: r.Name, Config: r.Config, Enabled: r.Enabled != 0,
		LastError: r.LastError,
	}
	var err error
	if out.LastSentAt, err = timePtr(r.LastSentAt); err != nil {
		return store.NotificationChannel{}, err
	}
	if out.CreatedAt, err = parseTime(r.CreatedAt); err != nil {
		return store.NotificationChannel{}, err
	}
	return out, nil
}

func (s notificationStore) CreateNotificationChannel(ctx context.Context, c store.NotificationChannel) error {
	return mapErr(s.q.CreateNotificationChannel(ctx, db.CreateNotificationChannelParams{
		ID: c.ID, OwnerID: c.OwnerID, Kind: c.Kind, Name: c.Name, Config: c.Config, Enabled: boolInt(c.Enabled),
		CreatedAt: formatTime(c.CreatedAt),
	}))
}

func (s notificationStore) GetNotificationChannel(ctx context.Context, ownerID, id string) (store.NotificationChannel, error) {
	r, err := s.q.GetNotificationChannel(ctx, db.GetNotificationChannelParams{OwnerID: ownerID, ID: id})
	if err != nil {
		return store.NotificationChannel{}, mapErr(err)
	}
	return toNotificationChannel(r)
}

func (s notificationStore) ListNotificationChannels(ctx context.Context, ownerID string) ([]store.NotificationChannel, error) {
	rows, err := s.q.ListNotificationChannels(ctx, ownerID)
	if err != nil {
		return nil, mapErr(err)
	}
	return convertAll(rows, toNotificationChannel)
}

func (s notificationStore) UpdateNotificationChannel(ctx context.Context, c store.NotificationChannel) error {
	n, err := s.q.UpdateNotificationChannel(ctx, db.UpdateNotificationChannelParams{
		Name: c.Name, Config: c.Config, Enabled: boolInt(c.Enabled), OwnerID: c.OwnerID, ID: c.ID,
	})
	return rowsOrNotFound(n, err)
}

func (s notificationStore) SetNotificationChannelResult(ctx context.Context, ownerID, id, lastError string, sentAt *time.Time) error {
	n, err := s.q.SetNotificationChannelResult(ctx, db.SetNotificationChannelResultParams{
		LastError: lastError, SentAt: nullTime(sentAt), OwnerID: ownerID, ID: id,
	})
	return rowsOrNotFound(n, err)
}

func (s notificationStore) DeleteNotificationChannel(ctx context.Context, ownerID, id string) error {
	n, err := s.q.DeleteNotificationChannel(ctx, db.DeleteNotificationChannelParams{OwnerID: ownerID, ID: id})
	return rowsOrNotFound(n, err)
}

func (s notificationStore) ListNotificationOwners(ctx context.Context) ([]store.NotificationOwner, error) {
	rows, err := s.q.ListNotificationOwners(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	return convertAll(rows, func(r db.ListNotificationOwnersRow) (store.NotificationOwner, error) {
		at, err := timePtr(r.LastRunAt)
		return store.NotificationOwner{OwnerID: r.OwnerID, LastRunAt: at}, err
	})
}

func (s notificationStore) GetNotificationSettings(ctx context.Context, ownerID string) (store.NotificationSettings, error) {
	r, err := s.q.GetNotificationSettings(ctx, ownerID)
	if err != nil {
		return store.NotificationSettings{}, mapErr(err)
	}
	at, err := timePtr(r.LastRunAt)
	if err != nil {
		return store.NotificationSettings{}, err
	}
	return store.NotificationSettings{
		OwnerID: r.OwnerID, Events: strings.Fields(r.Events), Threshold: r.Threshold, OpenDays: int(r.OpenDays),
		Language: r.Language, LastRunAt: at,
	}, nil
}

func (s notificationStore) SaveNotificationSettings(ctx context.Context, ns store.NotificationSettings) error {
	return mapErr(s.q.SaveNotificationSettings(ctx, db.SaveNotificationSettingsParams{
		OwnerID: ns.OwnerID, Events: strings.Join(ns.Events, " "), Threshold: ns.Threshold, OpenDays: int64(ns.OpenDays),
		Language: ns.Language,
	}))
}

func (s notificationStore) SetNotificationLastRun(ctx context.Context, ownerID string, at time.Time) error {
	n, err := s.q.SetNotificationLastRun(ctx, db.SetNotificationLastRunParams{
		LastRunAt: nullTime(&at), OwnerID: ownerID,
	})
	return rowsOrNotFound(n, err)
}

func (s notificationStore) ListNotificationStates(ctx context.Context, ownerID string) ([]store.NotificationState, error) {
	rows, err := s.q.ListNotificationStates(ctx, ownerID)
	if err != nil {
		return nil, mapErr(err)
	}
	return convertAll(rows, func(r db.NotificationState) (store.NotificationState, error) {
		at, err := parseTime(r.UpdatedAt)
		return store.NotificationState{Key: r.Key, State: r.State, UpdatedAt: at}, err
	})
}

func (s notificationStore) SetNotificationState(ctx context.Context, ownerID string, st store.NotificationState) error {
	return mapErr(s.q.SetNotificationState(ctx, db.SetNotificationStateParams{
		OwnerID: ownerID, Key: st.Key, State: st.State, UpdatedAt: formatTime(st.UpdatedAt),
	}))
}

func (s notificationStore) DeleteNotificationState(ctx context.Context, ownerID, key string) error {
	return mapErr(s.q.DeleteNotificationState(ctx, db.DeleteNotificationStateParams{OwnerID: ownerID, Key: key}))
}

func (s notificationStore) DeleteNotificationStates(ctx context.Context, ownerID string) error {
	return mapErr(s.q.DeleteNotificationStates(ctx, ownerID))
}
