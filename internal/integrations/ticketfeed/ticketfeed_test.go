package ticketfeed

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/jameynakama/whatnow/internal/integrations"
	"github.com/jameynakama/whatnow/internal/outbound"
)

type sent struct {
	count int
	last  *http.Request
}

// serve answers every request with status and body, and records what it was sent.
func serve(t *testing.T, status int, body string) (*httptest.Server, *sent) {
	t.Helper()
	got := &sent{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.count++
		got.last = r.Clone(context.Background())
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func cfg(addr, token string) integrations.Config {
	return integrations.Config{Values: map[string]string{"url": addr, "token": token}}
}

func pull(t *testing.T, c integrations.Config) integrations.Source {
	t.Helper()
	sources := New(nil).Pull(context.Background(), c, time.Time{}, time.Time{})
	if len(sources) != 1 || sources[0].Label != "Ticket feed" {
		t.Fatalf("sources = %+v", sources)
	}
	return sources[0]
}

// numbered returns n tickets with distinct keys, from ACME-1.
func numbered(n int) []integrations.Ticket {
	tickets := []integrations.Ticket{}
	for i := 1; i <= n; i++ {
		key := fmt.Sprintf("ACME-%d", i)
		tickets = append(tickets, integrations.Ticket{Key: key, Title: "Ticket " + key})
	}
	return tickets
}

func feedBody(t *testing.T, tickets []integrations.Ticket, more int) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"tickets": tickets, "more": more})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// failure is a non-200's body with text as its error.
func failure(text string) string {
	body, _ := json.Marshal(map[string]string{"error": text})
	return string(body)
}

// padded is an empty feed of exactly n bytes, so only its size can fail a pull.
func padded(n int) string {
	const empty = `{"tickets": []}`
	return empty + strings.Repeat(" ", n-len(empty))
}

// A view shows only whether a secret is set, so both fields must be secrets for "Secrets are
// never shown", and only the address may be required for "Ready with only an address".
func TestBothFieldsAreSecretAndOnlyTheAddressIsRequired(t *testing.T) {
	required := map[string]bool{}
	for _, f := range New(nil).Fields() {
		if f.Kind != integrations.Secret {
			t.Errorf("%s is %s, want %s", f.Key, f.Kind, integrations.Secret)
		}
		required[f.Key] = f.Required
	}
	if diff := cmp.Diff(map[string]bool{"url": true, "token": false}, required); diff != "" {
		t.Errorf("required (-want +got):\n%s", diff)
	}
}

func TestPullSendsOneGETToTheAddressAsGiven(t *testing.T) {
	cases := map[string]struct {
		user   string
		target string
		token  string
		auth   []string
	}{
		"Pull with a token":                               {target: "/tickets", token: "t0k", auth: []string{"Bearer t0k"}},
		"Pull without a token":                            {target: "/api/feed.json?team=acme"},
		"A user and password in the address are not sent": {user: "me:hunter2@", target: "/tickets"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			srv, got := serve(t, http.StatusOK, `{"tickets": []}`)
			addr := strings.Replace(srv.URL, "//", "//"+c.user, 1) + c.target
			if src := pull(t, cfg(addr, c.token)); src.Err != nil {
				t.Fatalf("err = %v", src.Err)
			}
			if got.count != 1 {
				t.Fatalf("the feed got %d requests, want 1", got.count)
			}
			r := got.last
			if r.Method != http.MethodGet || r.RequestURI != c.target || r.Header.Get("Accept") != "application/json" || !slices.Equal(c.auth, r.Header.Values("Authorization")) {
				t.Errorf("got %s %s, Accept %q, Authorization %q", r.Method, r.RequestURI, r.Header.Get("Accept"), r.Header.Values("Authorization"))
			}
		})
	}
}

func TestPullFollowsNoRedirect(t *testing.T) {
	clients := map[string]*http.Client{
		"Its own client":  nil,
		"A shared client": {Timeout: 30 * time.Second},
	}
	for name, client := range clients {
		t.Run(name, func(t *testing.T) {
			elsewhere, got := serve(t, http.StatusOK, `{"tickets": []}`)
			srv := httptest.NewServer(http.RedirectHandler(elsewhere.URL+"/tickets", http.StatusFound))
			t.Cleanup(srv.Close)
			sources := New(client).Pull(context.Background(), cfg(srv.URL+"/s3cr3t/tickets", ""), time.Time{}, time.Time{})
			if src := sources[0]; src.Err == nil || src.Err.Error() != "The ticket feed answered 302" || src.Tickets != nil {
				t.Errorf("err %v, tickets %+v", src.Err, src.Tickets)
			}
			if got.count != 0 {
				t.Errorf("the pull followed the redirect, with Referer %q", got.last.Header.Get("Referer"))
			}
			if client != nil && client.CheckRedirect != nil {
				t.Error("New changed the shared client")
			}
		})
	}
}

func TestAddressIsNotAURL(t *testing.T) {
	// The last one fails to parse with an error that repeats the address.
	for _, addr := range []string{"localhost", "localhost:8080/tickets", "https://", "http://127.0.0.1:8080/s3cr3t/%zz"} {
		src := pull(t, cfg(addr, ""))
		if src.Err == nil || src.Err.Error() != "that address isn't a URL, such as http://localhost:8080/tickets" || src.Tickets != nil {
			t.Errorf("%s: err %v, tickets %+v", addr, src.Err, src.Tickets)
		}
	}
}

func TestPullOffersTheFeedsTickets(t *testing.T) {
	const acme7 = `"key": "ACME-7", "title": "Fix the login", "notes": "Seen on staging", "url": "https://tracker.example.com/ACME-7", "rereview": true`
	seven := []integrations.Ticket{{Key: "ACME-7", Title: "Fix the login", Notes: "Seen on staging", URL: "https://tracker.example.com/ACME-7", Rereview: true}}
	cases := map[string]struct {
		body string
		want []integrations.Ticket
	}{
		"Tickets reach the pick list as sent": {body: `{"tickets": [{` + acme7 + `}]}`, want: seven},
		"Unknown fields are ignored":          {body: `{"generated_at": "2026-11-09T09:00:00Z", "tickets": [{` + acme7 + `, "priority": "high"}]}`, want: seven},
		"Empty feed":                          {body: `{"tickets": []}`, want: []integrations.Ticket{}},
		"No link":                             {body: `{"tickets": [{"key": "ACME-8", "title": "Write the release notes"}]}`, want: []integrations.Ticket{{Key: "ACME-8", Title: "Write the release notes"}}},
		"An answer of exactly 8 MiB":          {body: padded(8 << 20), want: []integrations.Ticket{}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _ := serve(t, http.StatusOK, c.body)
			src := pull(t, cfg(srv.URL, ""))
			if diff := cmp.Diff(c.want, src.Tickets); diff != "" || src.Err != nil || src.More != 0 {
				t.Errorf("err %v, more %d, tickets (-want +got):\n%s", src.Err, src.More, diff)
			}
		})
	}
}

func TestAnUnreadableAnswerOffersNothing(t *testing.T) {
	cases := map[string]string{
		"JSON without tickets":      `{"status": "ok"}`,
		"Null tickets":              `{"tickets": null}`,
		"A bare array":              `[{"key": "ACME-7", "title": "Fix the login"}]`,
		"A fractional more":         `{"tickets": [], "more": 1.5}`,
		"A more too large to count": feedBody(t, numbered(51), math.MaxInt),
		"Not JSON":                  "<!doctype html><title>Sign in</title>",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _ := serve(t, http.StatusOK, body)
			src := pull(t, cfg(srv.URL, ""))
			if src.Err == nil || src.Err.Error() != "couldn't read the ticket feed's answer — is the address right?" || src.Tickets != nil {
				t.Errorf("err %v, tickets %+v", src.Err, src.Tickets)
			}
		})
	}
}

func TestABadTicketOffersNone(t *testing.T) {
	script := func(key string) integrations.Ticket {
		return integrations.Ticket{Key: key, Title: "Open the report", URL: "javascript:alert(1)"}
	}
	cases := map[string]struct {
		tickets []integrations.Ticket
		want    string
	}{
		"A ticket without a title": {tickets: append(numbered(2), integrations.Ticket{Key: "ACME-3", Title: " "}), want: "every ticket needs a key and a title"},
		"A script link":            {tickets: []integrations.Ticket{script("ACME-9")}, want: "ACME-9's address isn't a web address"},
		// The fold would offer neither of these, but every ticket is checked first.
		"A bad ticket past the cap": {tickets: append(numbered(50), script("ACME-51")), want: "ACME-51's address isn't a web address"},
		"A bad repeat":              {tickets: append(numbered(1), script("acme-1")), want: "acme-1's address isn't a web address"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _ := serve(t, http.StatusOK, feedBody(t, c.tickets, 0))
			src := pull(t, cfg(srv.URL, ""))
			if src.Err == nil || src.Err.Error() != c.want || src.Tickets != nil || src.More != 0 {
				t.Errorf("err %v, tickets %+v, more %d; want %q", src.Err, src.Tickets, src.More, c.want)
			}
		})
	}
}

func TestRepeatsAndTheCap(t *testing.T) {
	again := func(key string) integrations.Ticket { return integrations.Ticket{Key: key, Title: "Again"} }
	cases := map[string]struct {
		tickets  []integrations.Ticket
		more     int
		want     []integrations.Ticket
		wantMore int
	}{
		"A key sent twice": {
			tickets: []integrations.Ticket{{Key: "acme-7", Title: "First"}, {Key: "ACME-8", Title: "Write the release notes"}, {Key: " ACME-7 ", Title: "Second"}},
			want:    []integrations.Ticket{{Key: "acme-7", Title: "First"}, {Key: "ACME-8", Title: "Write the release notes"}},
		},
		"Over the cap":                    {tickets: numbered(53), more: 10, want: numbered(50), wantMore: 13},
		"A repeat is not counted as more": {tickets: append(numbered(50), again("ACME-1")), want: numbered(50)},
		"A repeat before the cap":         {tickets: slices.Insert(numbered(51), 1, again("acme-1")), want: numbered(50), wantMore: 1},
		// ACME-51 is past the cap, and its repeat counts toward more only once.
		"A repeat of a ticket past the cap":                    {tickets: append(numbered(51), again(" acme-51")), want: numbered(50), wantMore: 1},
		"Negative more":                                        {tickets: numbered(2), more: -4, want: numbered(2)},
		"A negative more does not cancel tickets past the cap": {tickets: numbered(53), more: -4, want: numbered(50), wantMore: 3},
		"The largest more":                                     {tickets: numbered(51), more: math.MaxInt - 1, want: numbered(50), wantMore: math.MaxInt},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _ := serve(t, http.StatusOK, feedBody(t, c.tickets, c.more))
			src := pull(t, cfg(srv.URL, ""))
			if diff := cmp.Diff(c.want, src.Tickets); diff != "" || src.Err != nil || src.More != c.wantMore {
				t.Errorf("err %v, more %d, want %d, tickets (-want +got):\n%s", src.Err, src.More, c.wantMore, diff)
			}
		})
	}
}

func TestFeedErrors(t *testing.T) {
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	cases := map[string]struct {
		addr   string
		token  string
		status int
		body   string
		want   string
	}{
		"Nothing answers":                        {addr: gone.URL + "/tickets", want: "Can't reach the ticket feed — is it running?"},
		"A non-200 answer with an error message": {status: http.StatusBadGateway, body: failure("GitHub rate limit reached"), want: "GitHub rate limit reached"},
		"A 401 with an error message":            {token: "t0k", status: http.StatusUnauthorized, body: failure("That token expired on 1 November"), want: "That token expired on 1 November"},
		"Token rejected":                         {token: "t0k", status: http.StatusUnauthorized, want: "The ticket feed rejected the token"},
		"Token needed":                           {status: http.StatusUnauthorized, want: "The ticket feed needs a token"},
		"Forbidden with a token":                 {token: "t0k", status: http.StatusForbidden, want: "The ticket feed answered 403"},
		"Forbidden without a token":              {status: http.StatusForbidden, want: "The ticket feed answered 403"},
		"Another non-200 answer":                 {status: http.StatusNotFound, body: "404 page not found", want: "The ticket feed answered 404"},
		"A blank error message":                  {status: http.StatusInternalServerError, body: failure(" "), want: "The ticket feed answered 500"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if c.addr == "" {
				srv, _ := serve(t, c.status, c.body)
				c.addr = srv.URL + "/tickets"
			}
			src := pull(t, cfg(c.addr, c.token))
			if src.Err == nil || src.Err.Error() != c.want || src.Tickets != nil || src.More != 0 {
				t.Errorf("err %v, tickets %+v, more %d; want %q", src.Err, src.Tickets, src.More, c.want)
			}
		})
	}
}

// The feed sends one byte past the limit and then stalls, as an endless answer would, so a read
// that waits for the end fails as slow. The short timeout makes that failure quick.
func TestAHugeAnswerIsNotReadToTheEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(padded(8<<20 + 1)))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	sources := New(&http.Client{Timeout: 5 * time.Second}).Pull(context.Background(), cfg(srv.URL, ""), time.Time{}, time.Time{})
	if src := sources[0]; src.Err == nil || src.Err.Error() != "The ticket feed's response was too large to read" || src.Tickets != nil || src.More != 0 {
		t.Errorf("err %v, tickets %+v, more %d", src.Err, src.Tickets, src.More)
	}
}

func TestTheFeedIsSlow(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"No answer": func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() },
		"Half an answer": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"tickets": [`))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		},
	}
	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(handler)
			t.Cleanup(srv.Close)
			sources := New(&http.Client{Timeout: 50 * time.Millisecond}).Pull(context.Background(), cfg(srv.URL, ""), time.Time{}, time.Time{})
			if src := sources[0]; src.Err == nil || src.Err.Error() != "The ticket feed took over 30 seconds — try again" || src.Tickets != nil {
				t.Errorf("err %v, tickets %+v", src.Err, src.Tickets)
			}
		})
	}
}

// Each feed is sent the token t0k, and its answer is built from what it was sent.
func TestErrorsNeverRevealASecret(t *testing.T) {
	cases := map[string]struct {
		target string
		status int
		// A case without a body has nothing listening at its address.
		body func(r *http.Request) string
		want string
	}{
		"The feed echoes the token": {
			target: "/tickets", status: http.StatusBadRequest,
			body: func(*http.Request) string { return failure("bad token t0k") }, want: "bad token …",
		},
		"The feed echoes each secret twice": {
			target: "/s3cr3t/tickets", status: http.StatusForbidden,
			body: func(*http.Request) string {
				return failure("t0k can't read /s3cr3t/tickets, and t0k can't list /s3cr3t/tickets")
			},
			want: "… can't read …, and … can't list …",
		},
		"The feed echoes its path": {
			target: "/s3cr3t/tickets", status: http.StatusNotFound,
			body: func(*http.Request) string { return failure("no route /s3cr3t/tickets") }, want: "no route …",
		},
		"The feed echoes its address": {
			target: "/s3cr3t/tickets", status: http.StatusNotFound,
			body: func(r *http.Request) string { return failure("nothing at http://" + r.Host + r.URL.Path) }, want: "nothing at …",
		},
		"The address carries the token": {
			target: "/t0k/tickets", status: http.StatusNotFound,
			body: func(r *http.Request) string { return failure("nothing at http://" + r.Host + r.URL.Path) }, want: "nothing at …",
		},
		"The feed echoes its path and query": {
			target: "/tickets?key=s3cr3t", status: http.StatusNotFound,
			body: func(r *http.Request) string { return failure("no route " + r.RequestURI) }, want: "no route …",
		},
		"The feed echoes its path escaped and decoded": {
			target: "/s3cr3t%20feed/tickets", status: http.StatusNotFound,
			body: func(r *http.Request) string { return failure(r.RequestURI + " is " + r.URL.Path) }, want: "… is …",
		},
		"A ticket's key is the token": {
			target: "/tickets", status: http.StatusOK,
			body: func(*http.Request) string {
				return `{"tickets": [{"key": "t0k", "title": "Open the report", "url": "javascript:alert(1)"}]}`
			},
			want: "…'s address isn't a web address",
		},
		"A secret address that does not answer": {target: "/s3cr3t/tickets", want: "Can't reach the ticket feed — is it running?"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				w.Write([]byte(c.body(r)))
			}))
			t.Cleanup(srv.Close)
			if c.body == nil {
				srv.Close()
			}
			addr := srv.URL + c.target
			src := pull(t, cfg(addr, "t0k"))
			if src.Err == nil {
				t.Fatalf("no error, tickets %+v", src.Tickets)
			}
			for _, secret := range []string{"t0k", addr, "s3cr3t"} {
				if strings.Contains(src.Err.Error(), secret) {
					t.Errorf("err %q reveals %q", src.Err, secret)
				}
			}
			if src.Err.Error() != c.want || src.Tickets != nil {
				t.Errorf("err %q, tickets %+v; want %q", src.Err, src.Tickets, c.want)
			}
		})
	}
}

// Hosted whatnow builds the feed on outbound's client, which won't dial a private address such as
// the test server's.
func TestHostedAndAPrivateAddress(t *testing.T) {
	srv, got := serve(t, http.StatusOK, `{"tickets": []}`)
	sources := New(outbound.Client(30*time.Second)).Pull(context.Background(), cfg(srv.URL+"/tickets", ""), time.Time{}, time.Time{})
	if src := sources[0]; src.Err == nil || src.Err.Error() != "Can't reach the ticket feed — is it running?" || src.Tickets != nil || src.More != 0 {
		t.Errorf("err %v, tickets %+v, more %d", src.Err, src.Tickets, src.More)
	}
	if got.count != 0 {
		t.Errorf("the feed got %d requests, want 0", got.count)
	}
}
