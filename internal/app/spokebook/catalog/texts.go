package catalog

import "github.com/spokebook/spokebook/internal/app/spokebook/i18n"

// BuiltinTexts returns title and instructions of a step, translated for a
// built-in step (builtinID set) while they are empty.
func BuiltinTexts(l *i18n.Localizer, builtinID, title, instructions string) (string, string) {
	if builtinID == "" {
		return title, instructions
	}
	if title == "" {
		title = l.T("catalog." + builtinID + ".title")
	}
	if instructions == "" {
		instructions = l.T("catalog." + builtinID + ".instructions")
	}
	return title, instructions
}
