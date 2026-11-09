// Package ticketfeed offers tickets from any program that answers a GET with whatnow's own ticket
// JSON: the {tickets, more} body that `whatnow jira push` sends.
package ticketfeed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/jameynakama/whatnow/internal/integrations"
)

// maxResults caps a pull, as every ticket integration's does.
const maxResults = 50

// maxBody bounds the answer. Fifty tickets with long notes stay far below it, and the server
// shouldn't hold a feed that never ends in memory.
const maxBody = 8 << 20

var (
	errNotAURL     = errors.New("that address isn't a URL, such as http://localhost:8080/tickets")
	errUnreachable = errors.New("Can't reach the ticket feed — is it running?")
	// errSlow is a timeout, which means the feed is running but slow, so it doesn't ask whether
	// the feed is running.
	errSlow       = errors.New("The ticket feed took over 30 seconds — try again")
	errTooLarge   = errors.New("The ticket feed's response was too large to read")
	errUnreadable = errors.New("couldn't read the ticket feed's answer — is the address right?")
	errRejected   = errors.New("The ticket feed rejected the token")
	// errNeedsToken is a 401 to a request that carried no token, so there was none to reject.
	errNeedsToken = errors.New("The ticket feed needs a token")
)

// Feed is the Ticket feed integration.
type Feed struct {
	client *http.Client
}

// New returns the integration. A nil client means one that gives up after 30 seconds. The feed
// never follows a redirect, which would send the address, and any secret in its path, to the next
// server as the Referer, so a redirect reads as any other non-200 answer. New uses a copy of the
// client, since hosted whatnow shares one with integrations that do follow redirects.
func New(client *http.Client) *Feed {
	own := http.Client{Timeout: 30 * time.Second}
	if client != nil {
		own = *client
	}
	own.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Feed{client: &own}
}

func (*Feed) ID() string                { return "ticketfeed" }
func (*Feed) Name() string              { return "Ticket feed" }
func (*Feed) Pulls() integrations.Pulls { return integrations.PullsTickets }

// Fields keeps the address a secret, as a calendar's iCal address is, since a feed can carry its
// token in its path.
func (*Feed) Fields() []integrations.Field {
	return []integrations.Field{
		{Key: "url", Label: "Address", Kind: integrations.Secret, Required: true, Help: "Where your feed answers, such as http://localhost:8080/tickets"},
		{Key: "token", Label: "Token", Kind: integrations.Secret, Help: "Optional: sent as a Bearer token"},
	}
}

// Pull reads the feed. The window is ignored: a ticket isn't for a day.
func (f *Feed) Pull(ctx context.Context, cfg integrations.Config, _, _ time.Time) []integrations.Source {
	src := integrations.Source{Label: "Ticket feed"}
	src.Tickets, src.More, src.Err = f.fetch(ctx, cfg.Values["url"], cfg.Values["token"])
	return []integrations.Source{src}
}

// answer is a feed's 200. Tickets is a pointer so that JSON without the array, such as another
// program's answer at a wrong address, isn't read as an empty feed.
type answer struct {
	Tickets *[]integrations.Ticket `json:"tickets"`
	More    int                    `json:"more"`
}

// fetch returns the feed's tickets, checked and folded, and how many more there were. Its errors
// never wrap the URL parser's or the client's, which repeat the address, and hide whatever the
// feed echoes of the token, the address, or its path.
func (f *Feed) fetch(ctx context.Context, addr, token string) ([]integrations.Ticket, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr, nil)
	if err != nil || req.URL.Scheme == "" || req.URL.Host == "" {
		return nil, 0, errNotAURL
	}
	// The client would send a user and password in the address as Basic auth, but a feed's only
	// credential is its token.
	req.URL.User = nil
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, 0, failed(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, 0, failed(err)
	}
	if len(body) > maxBody {
		return nil, 0, errTooLarge
	}
	hide := redactor(req.URL, addr, token)
	if resp.StatusCode != http.StatusOK {
		var bad struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &bad) == nil {
			if msg := strings.TrimSpace(bad.Error); msg != "" {
				return nil, 0, errors.New(hide(msg))
			}
		}
		if resp.StatusCode == http.StatusUnauthorized {
			if token != "" {
				return nil, 0, errRejected
			}
			return nil, 0, errNeedsToken
		}
		return nil, 0, fmt.Errorf("The ticket feed answered %d", resp.StatusCode)
	}
	var res answer
	if err := json.Unmarshal(body, &res); err != nil || res.Tickets == nil {
		return nil, 0, errUnreadable
	}
	// Every ticket is checked before the fold, so a bad one past the cap or behind a repeated key
	// still fails the pull.
	for _, tk := range *res.Tickets {
		if err := integrations.CheckTicket(tk); err != nil {
			// The check's words name the ticket's key, which the feed chose.
			return nil, 0, errors.New(hide(err.Error()))
		}
	}
	tickets, overflow := fold(*res.Tickets)
	more := max(res.More, 0)
	// A total too large for an int would wrap negative. It's unreadable, as a more too large to
	// decode already is.
	if more > math.MaxInt-overflow {
		return nil, 0, errUnreadable
	}
	return tickets, overflow + more, nil
}

// failed is the error for a request or a read that got no whole answer.
func failed(err error) error {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return errSlow
	}
	return errUnreachable
}

// redactor returns a function that replaces every copy of the token, the address, and the
// address's path with "…" in words the feed chose, which could echo what it was sent. A feed may
// echo its path with the query, as sent, or decoded, so each form is replaced. A path of just "/"
// holds no secret, and replacing it would mangle the words.
func redactor(u *url.URL, addr, token string) func(string) string {
	secrets := []string{token, addr}
	for _, p := range []string{u.RequestURI(), u.EscapedPath(), u.Path} {
		if len(p) > 1 {
			secrets = append(secrets, p)
		}
	}
	// Replacing "" would put "…" between every rune.
	secrets = slices.DeleteFunc(secrets, func(s string) bool { return s == "" })
	// Longest first, so a secret inside another, such as a token in the path or a path in the
	// address, is hidden whole with it rather than leaving the rest of the other one showing.
	slices.SortFunc(secrets, func(a, b string) int { return len(b) - len(a) })
	return func(text string) string {
		for _, s := range secrets {
			text = strings.ReplaceAll(text, s, "…")
		}
		return text
	}
}

// fold keeps the first ticket for each key, compared trimmed and upper-cased as a ticket's key is
// to a task's ref. It offers the first maxResults of them in the feed's order and counts the rest.
// A dropped repeat is neither offered nor counted.
func fold(tickets []integrations.Ticket) ([]integrations.Ticket, int) {
	offered := []integrations.Ticket{}
	seen := map[string]bool{}
	overflow := 0
	for _, tk := range tickets {
		key := strings.ToUpper(strings.TrimSpace(tk.Key))
		if seen[key] {
			continue
		}
		seen[key] = true
		if len(offered) == maxResults {
			overflow++
			continue
		}
		offered = append(offered, tk)
	}
	return offered, overflow
}
