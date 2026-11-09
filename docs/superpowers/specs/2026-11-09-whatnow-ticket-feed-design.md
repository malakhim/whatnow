# whatnow integrations: Ticket feed

The Ticket feed is a ticket integration beside Jira, Linear, and Review Queue, on the contract in
`2026-10-12-whatnow-integrations-design.md` and `2026-10-12-whatnow-jira-design.md`. It speaks no
service's API. It reads one address that answers with whatnow's own ticket JSON, so a source
whatnow has no package for (Jira Cloud, the pull requests the user opened, a local tool's task
list) needs only a small program that serves that shape. Everything a ticket integration gets
there (the pick list, skip, dedupe by ref, `rereview`, Add, the TUI step) the feed gets unchanged.
This spec covers only what differs.

## Behavior

- **Any program can be a feed:** whatever answers a `GET` at the saved address with the JSON in the
  contract below, such as a file a scheduled job publishes to a static host, a small server on
  `127.0.0.1`, or a tool that merges several sources into one list. whatnow adds no path, so the
  address can end in `/tickets`, `/api/feed.json`, or a file's name.
- **The feed decides how its tickets read.** Titles show as sent, in the feed's order. Jira and
  Linear put the key first; a feed that wants that puts the key in its titles, so one that already
  does isn't shown as "ACME-7 ACME-7 Fix the login".
- **One bad ticket fails the pull,** as it fails a push, and the error names the check. A broken
  feed then reads as broken rather than as a short list. Offering the good tickets with a warning
  would need somewhere new to show it: the pick list shows a source's error or its tickets.
- **A key listed twice is offered once,** the first time. A feed that merges sources may list one
  ref from two of them, which isn't a bug, and the pick list keys its rows by `key`.
- **The rest of the ticket flow is unchanged.** A pull offers and adds nothing. Add makes a task of
  each picked ticket, with its key and link as the ref. × skips a key for good. A key already on a
  task isn't offered, unless that task is done or dropped and the feed sets `rereview`.
- **One feed per install.** A user with several sources merges them into one feed. A list of feeds,
  like the calendar's list of calendars, would need a secret and a label per row; it can come later
  by moving the saved address into a first row.
- **Local and hosted, never "From my computer".** Hosted whatnow reaches only public addresses
  through the feed, as with the calendar, Jira, and Linear.

## Ticket feed

`internal/integrations/ticketfeed`, ID `ticketfeed`, name "Ticket feed", built like `reviewqueue`,
knowing nothing of whatnow's storage. The name says what it brings, so the TUI asks "Pull from Jira
and Ticket feed?"; "JSON feed" would name the format, and could read as a calendar's feed.

- **Fields:** `url` (secret, required; label "Address"; help: "Where your feed answers, such as
  http://localhost:8080/tickets") and `token` (secret, optional; label "Token"; help: "Optional:
  sent as a Bearer token"). The address is a secret, as a calendar's iCal address is, because a
  feed can carry its token in its path. Settings show that each is set and never show either. The
  address alone makes the feed ready to pull.
- **Auth is optional.** A saved token is sent as `Authorization: Bearer <token>`. With none, the
  request has no `Authorization` header: a feed on `127.0.0.1` usually needs none, and a required
  token would force a dummy value.
- **One request per pull:** a `GET` to the address exactly as saved, with no path added, sending
  `Accept: application/json`. It gives up after 30 seconds, as Jira and Linear do, and `New(nil)`
  builds that client. Review Queue's minute suits an organization-wide scan, but the morning step
  shouldn't wait that long on a feed that hangs.
- **The answer is read to at most 8 MiB,** through `io.LimitReader`. Fifty tickets with long notes
  stay far below it, and on a shared server one feed that streams without end would otherwise use
  up memory for every user.
- **A `200` decodes into** `struct { Tickets *[]integrations.Ticket; More int }`. The pointer tells
  a missing or `null` `tickets` from `[]`, so a wrong address that answers other JSON, such as a
  health check, is unreadable rather than a feed with nothing new.
- **Every ticket is checked** with `integrations.CheckTicket` (see the contract) before any is
  offered.
- **Then repeats and the cap,** in the feed's order. Keys compare trimmed and upper-cased, as a
  ticket's key is matched to a task's ref, and the first ticket with a key wins. The first 50
  distinct tickets are offered, the cap every ticket integration has. `More` is the distinct
  tickets past the cap plus the feed's own `more`, where a missing or negative `more` counts as 0.
  A dropped repeat counts toward neither. The pick list shows `More` as its usual "N more matched —
  narrow the search". Refusing an answer over 50, as a push does, wouldn't help: a pull can't ask
  for fewer.
- **Errors never contain the token, the address, or the address's path,** and none wraps an error
  from Go's URL parser or HTTP client, which repeat the address. Each failure is one sentence, and
  the pull offers no tickets:
  - an address with no scheme or host:
    "that address isn't a URL, such as http://localhost:8080/tickets"
  - no response: "Can't reach the ticket feed — is it running?"
  - a timeout: "The ticket feed took over 30 seconds — try again", since a slow feed is running
  - a body over 8 MiB: "The ticket feed's response was too large to read"
  - a non-`200` with an `error`: that text, as Review Queue shows its own, so a `502` with
    `{"error": "GitHub rate limit reached"}` reads "GitHub rate limit reached". First, every copy
    of the saved token, the address, and the address's path (when longer than `/`) becomes `…`.
    The feed is outside whatnow's control and could echo what it was sent.
  - a `401` without one: "The ticket feed rejected the token" when a token is saved, and
    "The ticket feed needs a token" when none is, since nothing was sent to reject
  - another non-`200` without one: "The ticket feed answered <status>". That includes `403`, as for
    Jira and Linear: the token usually works but lacks access, so "rejected" would point at the
    wrong fix.
  - a `200` that isn't the contract's JSON, such as a sign-in page, or JSON with no `tickets` array:
    "couldn't read the ticket feed's answer — is the address right?"
  - a ticket that fails the check: the check's own words, such as
    "every ticket needs a key and a title" or "ACME-9's address isn't a web address"

**A picked ticket's task:** as a Jira ticket's. `title` and `notes` are the feed's, `refs` is one
`{label: key, url}`, size and energy stay blank, and it skips the model's cleanup.

## The contract

- **A feed answers what `whatnow jira push` sends:** `{tickets, more}`, whose tickets are
  `Ticket`'s JSON. whatnow already sends and receives this body for pushed Jira tickets, so it keeps
  one shape for a list of tickets:

  ```json
  {
    "tickets": [
      {
        "key": "ACME-7",
        "title": "Fix the login",
        "notes": "Seen on staging",
        "url": "https://tracker.example.com/ACME-7",
        "rereview": true
      },
      { "key": "ACME-8", "title": "Write the release notes" }
    ],
    "more": 12
  }
  ```

  - `tickets` (required, may be empty): each ticket reaches the pick list as sent, in the feed's
    order. `key` and `title` are required, and `key` becomes the task's ref, which links and dedupes
    it. `notes` becomes the task's notes. `url`, when present, is an `http` or `https` link.
    `rereview` means the source is asking for this again, so a done or dropped task with this key
    doesn't hide it (`2026-10-15-whatnow-review-queue-design.md`).
  - `more` (optional, a whole number): how many the feed left out, for a feed that trims its own
    list. Missing is 0, so `{"tickets": [...]}` alone is a whole answer.
  - Fields whatnow doesn't know, on a ticket or on the answer, are ignored, so a feed can carry its
    own.
  - A failure is any non-`200`, optionally with `{"error": "..."}` in words for the user.
- **Push's check is shared.** `checkTicket` in `internal/integrations/push.go` becomes
  `CheckTicket`, unchanged, so a push and a feed refuse the same tickets in the same words: a blank
  key or title, or a link that isn't `http` or `https`, such as a `javascript:` one that would run
  as script when clicked. Push behaves as before. `Runner.offer` doesn't run the check, so Jira,
  Linear, and Review Queue pulls don't change here.

## Wiring

The feed is in both lists in `internal/app/app.go`: local as `ticketfeed.New(nil)`, after Review
Queue, and hosted as `ticketfeed.New(guarded)`, after Linear. `guarded` is
`outbound.Client(30 * time.Second)`, which checks the address of every dial and redirect. A hosted
pull from a private address, such as `http://127.0.0.1:8080/tickets`, therefore sends nothing and
reads "Can't reach the ticket feed — is it running?", as Jira and Linear give their own unreachable
message there. Review Queue stays local-only, since it always runs on the user's own network; a feed
can live anywhere public.

The feed isn't `Pushable`: `SetVia` and `Receive` refuse it with `ErrInvalid`, and its card never
offers "From my computer". Push's CLI and key routes are Jira's, so a general push is its own
change.

No other code changes: not the runner, the HTTP API, the stored settings, the web client, or the
TUI. The settings card and the ticket step render from `Fields()` and `Name()`.

## Testing

- **Go, `ticketfeed` over `httptest`:** both fields secret and only the address required; the
  request (the path as saved, `Accept`, the Bearer header with a token, and no `Authorization`
  without one); a ticket's fields as sent, unknown fields ignored, an empty feed, JSON without
  `tickets`, and non-JSON; the check (a blank title among good tickets, a `javascript:` link, no
  link); repeats and the cap (a key sent twice in another case and with spaces, 53 tickets with a
  `more`, a repeat past the cap, a negative `more`); and each error: not a URL, unreachable, slow
  (a short client timeout), over 8 MiB, a `502` with an `error`, `401` with and without a token,
  `403`, and `404`. The secret tests save a token and an address with a secret path, have the feed
  echo them back or not answer at all, and search the error for each.
- **Go, the guard:** a feed built on `outbound.Client` and pointed at an `httptest` server on
  `127.0.0.1` sends it nothing, and reports "Can't reach the ticket feed — is it running?".
- **Go, `app`:** local and hosted whatnow both offer `ticketfeed`, and its view isn't `pushable`.
- **Go, `integrations`:** push's tests pass unchanged after the rename. The runner's tests already
  cover what a pulled ticket meets next (dedupe by ref, skip, `rereview` past a done task, Add), so
  the feed adds none there.
- **Playwright:** `ticketfeed.spec.ts` runs a fake feed from Node on an ephemeral `127.0.0.1` port,
  which the test enters as the address, so `playwright.config.ts` doesn't change. The fake answers
  two tickets and records the `Authorization` header. The flow saves the address and a token,
  switches the feed on, pulls from the morning, skips one ticket, adds the other, and pulls again:
  neither comes back, and the fake saw `Bearer <token>`.
