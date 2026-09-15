package mailfmt

import (
	"fmt"

	"github.com/kpenfound/busybees/internal/ghwork"
	"github.com/kpenfound/busybees/internal/mail"
)

// FormatMail supplies GitHub display fields to the tracker-independent formatter.
func FormatMail(m mail.Message) string {
	var fields []mail.Field
	if n := ghwork.Issue(m.Work); n > 0 {
		fields = append(fields, mail.Field{Name: "issue", Value: fmt.Sprintf("#%d", n)})
	}
	if n := ghwork.PR(m.Work); n > 0 {
		fields = append(fields, mail.Field{Name: "pr", Value: fmt.Sprintf("#%d", n)})
	}
	return mail.Format(m, fields...)
}
