package server_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/spokebook/spokebook/internal/app/spokebook/auth"
)

func TestBikeSerialNumber(t *testing.T) {
	c := newClient(t)
	_, _, body := c.do("GET", "/bikes/new", nil)
	mustContain(t, body, "Rahmennummer", `name="serial_number" autocomplete="off" value=""`)

	// A validation error keeps the submitted serial number.
	status, _, body := c.do("POST", "/bikes", url.Values{"name": {""}, "serial_number": {"WTU123"}})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("missing name: status %d", status)
	}
	mustContain(t, body, `name="serial_number" autocomplete="off" value="WTU123"`)

	// Stored trimmed and shown on the bike page.
	status, bikePath, body := c.do("POST", "/bikes", url.Values{"name": {"Gravel"}, "serial_number": {"  WTU 123 <b>  "}})
	if status != http.StatusOK || !strings.HasPrefix(bikePath, "/bikes/") {
		t.Fatalf("create: %d %s", status, bikePath)
	}
	mustContain(t, body, "<dt>Rahmennummer</dt><dd>WTU 123 &lt;b&gt;</dd>")
	id := strings.TrimPrefix(bikePath, "/bikes/")
	b, err := c.st.Bikes().GetBike(context.Background(), auth.FirstUserID, id)
	if err != nil || b.SerialNumber != "WTU 123 <b>" {
		t.Fatalf("stored %q, %v", b.SerialNumber, err)
	}

	// Edit: prefilled, changed, then cleared.
	_, _, body = c.do("GET", bikePath+"/edit", nil)
	mustContain(t, body, `name="serial_number" autocomplete="off" value="WTU 123 &lt;b&gt;"`)
	_, _, body = c.do("POST", bikePath, url.Values{"name": {"Gravel"}, "serial_number": {"WTU999"}})
	mustContain(t, body, "<dt>Rahmennummer</dt><dd>WTU999</dd>")
	// A failed edit keeps the newly submitted value, not the stored one.
	status, _, body = c.do("POST", bikePath, url.Values{"name": {"Gravel"}, "origin": {"stolen"}, "serial_number": {"NEW1"}})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("invalid origin: %d", status)
	}
	mustContain(t, body, `name="serial_number" autocomplete="off" value="NEW1"`)
	_, _, body = c.do("GET", bikePath, nil)
	mustContain(t, body, "<dd>WTU999</dd>")

	_, _, body = c.do("POST", bikePath, url.Values{"name": {"Gravel"}, "serial_number": {"   "}})
	mustNotContain(t, body, "Rahmennummer", "WTU999")
	if b, _ := c.st.Bikes().GetBike(context.Background(), auth.FirstUserID, id); b.SerialNumber != "" {
		t.Errorf("not cleared: %q", b.SerialNumber)
	}

	// Without a serial number, the bike page has no such entry.
	_, _, body = c.do("POST", "/bikes", url.Values{"name": {"Road"}})
	mustNotContain(t, body, "Rahmennummer")

	// English label.
	c.lang = "en"
	c.do("POST", bikePath, url.Values{"name": {"Gravel"}, "serial_number": {"EN1"}})
	_, _, body = c.do("GET", bikePath, nil)
	mustContain(t, body, "<dt>Frame number</dt><dd>EN1</dd>")
}

func TestComponentSerialNumber(t *testing.T) {
	c := newClient(t)
	_, _, body := c.do("GET", "/components/new", nil)
	mustContain(t, body, "Seriennummer", `name="serial_number" autocomplete="off" value=""`)

	// A validation error keeps the submitted serial number.
	status, _, body := c.do("POST", "/components", url.Values{"type": {"chain"}, "price": {"x"}, "serial_number": {"SN-1"}})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("invalid price: status %d", status)
	}
	mustContain(t, body, `name="serial_number" autocomplete="off" value="SN-1"`)

	status, compPath, body := c.do("POST", "/components", url.Values{
		"type": {"fork"}, "manufacturer": {"Fox"}, "part_number": {"910-20"}, "serial_number": {"  F0X-42  "},
	})
	if status != http.StatusOK || !strings.HasPrefix(compPath, "/components/") {
		t.Fatalf("create: %d %s", status, compPath)
	}
	mustContain(t, body, "<dt>Artikelnummer</dt><dd>910-20</dd>", "<dt>Seriennummer</dt><dd>F0X-42</dd>")
	id := strings.TrimPrefix(compPath, "/components/")
	if got, err := c.st.Components().GetComponent(context.Background(), auth.FirstUserID, id); err != nil || got.SerialNumber != "F0X-42" {
		t.Fatalf("stored %q, %v", got.SerialNumber, err)
	}

	// Edit: prefilled, changed, kept on a validation error, cleared.
	_, _, body = c.do("GET", compPath+"/edit", nil)
	mustContain(t, body, `name="serial_number" autocomplete="off" value="F0X-42"`)
	_, _, body = c.do("POST", compPath, url.Values{"type": {"fork"}, "manufacturer": {"Fox"}, "serial_number": {"F0X-43"}})
	mustContain(t, body, "<dt>Seriennummer</dt><dd>F0X-43</dd>")
	status, _, body = c.do("POST", compPath, url.Values{"type": {"fork"}, "price": {"x"}, "serial_number": {"F0X-44"}})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("invalid price on edit: status %d", status)
	}
	mustContain(t, body, `name="serial_number" autocomplete="off" value="F0X-44"`)
	_, _, body = c.do("POST", compPath, url.Values{"type": {"fork"}, "manufacturer": {"Fox"}, "serial_number": {""}})
	mustNotContain(t, body, "Seriennummer", "F0X-4")

	// Without a serial number, the component page has no such entry.
	_, _, body = c.do("POST", "/components", url.Values{"type": {"chain"}})
	mustNotContain(t, body, "Seriennummer")
}

func TestSerialNumberAddDuplicateReplace(t *testing.T) {
	c := newClient(t)
	bikePath := c.createBike("Gravel")
	c.do("POST", bikePath+"/odometer", url.Values{"date": {"2026-01-01"}, "km": {"100"}})

	// Installing a new component directly on the bike stores its serial number.
	_, _, body := c.do("GET", bikePath+"/components/new", nil)
	mustContain(t, body, `name="serial_number" autocomplete="off" value=""`)
	status, path, body := c.do("POST", bikePath+"/components", url.Values{
		"date": {"2026-02-01"}, "position": {"rear"}, "type": {"wheel"}, "manufacturer": {"DT Swiss"},
		"part_number": {"GR1600"}, "serial_number": {" DT-0001 "}, "dealer": {"Rose"},
	})
	if status != http.StatusOK || path != bikePath+"/components" {
		t.Fatalf("add to bike: %d %s", status, path)
	}
	compPath := firstLink(t, body, "/components/")
	_, _, body = c.do("GET", compPath, nil)
	mustContain(t, body, "<dt>Seriennummer</dt><dd>DT-0001</dd>")

	// Duplicating copies the part number, not the serial number.
	_, _, body = c.do("GET", compPath+"/duplicate", nil)
	mustContain(t, body, `name="duplicate_of"`, `value="GR1600"`, `value="Rose"`, `name="serial_number" autocomplete="off" value=""`)
	mustNotContain(t, body, "DT-0001")

	// Replacing: the form does not copy the old serial number, the new
	// component stores the submitted one and the old one keeps its own.
	_, _, body = c.do("GET", compPath+"/replace", nil)
	mustContain(t, body, `value="GR1600"`, `value="Rose"`, `name="serial_number" autocomplete="off" value=""`)
	mustNotContain(t, body, "DT-0001")
	status, path, body = c.do("POST", compPath+"/replace", url.Values{
		"date": {"2026-06-01"}, "position": {"rear"}, "type": {"wheel"}, "manufacturer": {"DT Swiss"},
		"part_number": {"GR1600"}, "serial_number": {"DT-0002"},
	})
	if status != http.StatusOK || path != bikePath+"/components" {
		t.Fatalf("replace: %d %s", status, path)
	}
	newPath := firstLink(t, body, "/components/")
	if newPath == compPath {
		t.Fatalf("first link is still the old component: %s", newPath)
	}
	_, _, body = c.do("GET", newPath, nil)
	mustContain(t, body, "<dd>DT-0002</dd>")
	_, _, body = c.do("GET", compPath, nil)
	mustContain(t, body, "<dd>DT-0001</dd>")
	mustNotContain(t, body, "DT-0002")

	// Replacing without a serial number leaves it empty.
	_, _, body = c.do("POST", newPath+"/replace", url.Values{
		"date": {"2026-07-01"}, "position": {"rear"}, "type": {"wheel"}, "manufacturer": {"Mavic"},
	})
	third := firstLink(t, body, "/components/")
	_, _, body = c.do("GET", third, nil)
	mustContain(t, body, "Mavic")
	mustNotContain(t, body, "Seriennummer")
}

// spareOption returns the text of the <option> for component id in body.
func spareOption(t *testing.T, body, id string) string {
	t.Helper()
	i := strings.Index(body, `<option value="`+id+`"`)
	if i < 0 {
		t.Fatalf("no option for %s", id)
	}
	rest := body[i:]
	start := strings.Index(rest, ">") + 1
	end := strings.Index(rest, "</option>")
	if end < start {
		t.Fatalf("unterminated option for %s", id)
	}
	return rest[start:end]
}

func TestSerialNumberSpareOptions(t *testing.T) {
	c := newClient(t)
	bikePath := c.createBike("Gravel")
	c.do("POST", bikePath+"/odometer", url.Values{"date": {"2026-01-01"}, "km": {"100"}})
	_, _, body := c.do("POST", bikePath+"/components", url.Values{
		"date": {"2026-01-01"}, "position": {"rear"}, "type": {"brake_pads"}, "manufacturer": {"Shimano"}, "model": {"L05A"},
	})
	compPath := firstLink(t, body, "/components/")

	spare := func(serial string) string {
		t.Helper()
		_, p, _ := c.do("POST", "/components", url.Values{
			"type": {"brake_pads"}, "manufacturer": {"Shimano"}, "model": {"L05A"}, "serial_number": {serial},
		})
		if !strings.HasPrefix(p, "/components/") {
			t.Fatalf("create spare: %s", p)
		}
		return strings.TrimPrefix(p, "/components/")
	}
	a, b, none := spare("SN-A"), spare("SN-<B>"), spare("")
	// A spare with manufacturer/model empty but a serial number.
	_, p, _ := c.do("POST", "/components", url.Values{"type": {"chain"}, "serial_number": {"CH-9"}})
	bare := strings.TrimPrefix(p, "/components/")

	for name, path := range map[string]string{"install": bikePath + "/components/install", "replace": compPath + "/replace?existing=1"} {
		t.Run(name, func(t *testing.T) {
			_, _, body := c.do("GET", path, nil)
			optA, optB, optNone, optBare := spareOption(t, body, a), spareOption(t, body, b), spareOption(t, body, none), spareOption(t, body, bare)
			if !strings.HasSuffix(optA, "Shimano L05A (SN-A)") {
				t.Errorf("spare A: %q", optA)
			}
			if !strings.HasSuffix(optB, "Shimano L05A (SN-&lt;B&gt;)") {
				t.Errorf("spare B (escaped): %q", optB)
			}
			if optA == optB || optA == optNone || optB == optNone {
				t.Errorf("options not distinguishable: %q %q %q", optA, optB, optNone)
			}
			if strings.Contains(optNone, "(") || !strings.HasSuffix(optNone, "Shimano L05A") {
				t.Errorf("spare without serial: %q", optNone)
			}
			if strings.Contains(optBare, ":") || !strings.HasSuffix(optBare, " (CH-9)") {
				t.Errorf("spare without manufacturer/model: %q", optBare)
			}
			mustNotContain(t, body, "()")
		})
	}
}
