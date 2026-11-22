package components

import (
	"github.com/spokebook/spokebook/internal/app/spokebook/i18n"
	"github.com/spokebook/spokebook/internal/app/spokebook/store"
)

// TypeName returns the name of a component type: its own name, else the
// translated name of the built-in type.
func TypeName(l *i18n.Localizer, t store.ComponentType) string {
	if t.Name != "" {
		return t.Name
	}
	return l.T("component.type." + t.ID)
}
