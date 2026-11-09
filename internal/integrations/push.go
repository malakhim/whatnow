package integrations

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jameynakama/whatnow/internal/service"
)

// How an integration is reached: by the server, or from the user's own computer, which sends what
// it pulled. The second is for a Jira the server can't see, behind a VPN or on an intranet.
const (
	ViaDirect   = "direct"
	ViaComputer = "computer"
)

// maxPushed caps a push at what a direct Jira pull returns.
const maxPushed = 50

// Pushed is the last list the user's computer sent, and when it arrived.
type Pushed struct {
	At      time.Time `json:"at"`
	Tickets []Ticket  `json:"tickets"`
	More    int       `json:"more"`
}

// SetVia sets how a ticket integration is reached.
func (r *Runner) SetVia(ctx context.Context, id, via string) (View, error) {
	in, err := r.find(id)
	if err != nil {
		return View{}, err
	}
	if via != ViaDirect && via != ViaComputer {
		return View{}, fmt.Errorf("%w: no way to reach %s called %q", service.ErrInvalid, in.Name(), via)
	}
	if err := needPushable(in); err != nil {
		return View{}, err
	}
	s, err := load(ctx, r.db, id)
	if err != nil {
		return View{}, err
	}
	s.Via = via
	if err := store(ctx, r.db, id, s); err != nil {
		return View{}, err
	}
	return r.view(ctx, in)
}

// Receive keeps the tickets the user's computer sent, in place of what it sent before. Nothing
// becomes a task until the user picks it from a pull.
func (r *Runner) Receive(ctx context.Context, id string, tickets []Ticket, more int) error {
	in, err := r.find(id)
	if err != nil {
		return err
	}
	if err := needPushable(in); err != nil {
		return err
	}
	s, err := load(ctx, r.db, id)
	if err != nil {
		return err
	}
	if !s.Enabled {
		return fmt.Errorf("%w: %s is off in whatnow settings — turn it on", service.ErrConflict, in.Name())
	}
	if s.Via != ViaComputer {
		return fmt.Errorf("%w: %s is set to reach it directly — switch it to From my computer", service.ErrConflict, in.Name())
	}
	if len(tickets) > maxPushed {
		return fmt.Errorf("%w: at most %d tickets at once", service.ErrInvalid, maxPushed)
	}
	for _, tk := range tickets {
		if err := CheckTicket(tk); err != nil {
			return fmt.Errorf("%w: %v", service.ErrInvalid, err)
		}
	}
	s.Pushed = &Pushed{At: r.now(), Tickets: tickets, More: more}
	return store(ctx, r.db, id, s)
}

func needPushable(in Integration) error {
	if _, ok := in.(Pushable); !ok {
		return fmt.Errorf("%w: %s can't be sent from your computer", service.ErrInvalid, in.Name())
	}
	return nil
}

// CheckTicket refuses a ticket the pick list couldn't show: one without a key or title, or with a
// link that isn't a web address, which would run as script when clicked.
func CheckTicket(tk Ticket) error {
	if strings.TrimSpace(tk.Key) == "" || strings.TrimSpace(tk.Title) == "" {
		return errors.New("every ticket needs a key and a title")
	}
	if tk.URL == "" {
		return nil
	}
	u, err := url.Parse(tk.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("%s's address isn't a web address", tk.Key)
	}
	return nil
}

// pushedSource is the last list the user's computer sent, as a pull's one source.
func pushedSource(in Integration, s saved) Source {
	if s.Pushed == nil {
		return Source{Label: in.Name(), Err: errors.New("Nothing sent from your computer yet — run `whatnow jira push`")}
	}
	return Source{Label: in.Name(), Tickets: s.Pushed.Tickets, More: s.Pushed.More}
}
