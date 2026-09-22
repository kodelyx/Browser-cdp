# cdp-control

An AI-first debugging engine. A calling agent uses it as its eyes and hands on a
live browser tab: drive the UI, read the network traffic the page actually sent,
and get either back as typed JSON from a local Needle 3 model — so the caller
writes code against ground truth instead of a guess.

It exists because debugging has to be separate from the thing being debugged.
Finding out what an app sends means driving its UI and reading its own network
traffic, and doing that from inside the product means every experiment runs against
the product's bridge, its scope, and its state. This is the same extension with
nothing else attached, scoped to nothing in particular.

## Run it

```bash
go build -o cdp-control ./src
go test ./src/...                   # run the test suite
./cdp-control                       # ws 9223, http 8201
./cdp-control -ws 127.0.0.1:9222    # when nothing else holds it
./cdp-control -http 127.0.0.1:8301  # when 8201 is taken
```

**Universal by default.** `-targets` and `-domains` are empty out of the box, and
the extension reads an empty allowlist as full access: every open tab is
attachable and every domain is in scope, like a raw `--remote-debugging-port`.
Scoping is still available when you want it, but it is a decision you make rather
than one this tool assumes.

Cookie mirroring (the persisted jar that lets a backend keep running after the
browser closes) is **off** when unscoped — there is no declared site to mirror, and
copying every cookie in the browser to disk is not a debugging tool's business.
Read cookies on demand through `/cookies` instead.

The default WebSocket port is **9223**, not the extension's own 9222, so this can run
alongside a product that drives the same extension. Both listen for a WebSocket and
both extensions dial 9222 by default, so sharing the port means whichever connects
first wins and the other silently gets the wrong bridge.

Point the extension you use for debugging at `ws://127.0.0.1:9223` — one line in its
`config.js` — and leave the other where it is.

Load `../extension` as an unpacked extension in Chrome. It dials out; there is
nothing to point at it.

## Endpoints

| Method | Path | What it does |
|---|---|---|
| GET | `/status` | Everything in one call: attached, which extension, what it can do, what it may reach, which tab |
| GET | `/health` | Just the attachment state |
| GET | `/requests?filter=&method=` | **The network requests the page made — `{method, url, body}`** |
| GET | `/events?filter=&type=` | Raw buffered CDP events |
| GET | `/tabs` | Open tabs |
| POST | `/eval` | `{"expression": "…"}` — run JS in the attached tab |
| POST | `/click` | `{"text": "Save"}` — click an element by its visible text |
| POST | `/cdp` | `{"method": "…", "params": {…}}` — any CDP command |
| GET | `/cookies?domain=&values=1` | Cookie names for a scope; values only when asked |
| POST | `/agent/act` | **AI drives the tab, then reads back the requests that action caused** |
| POST | `/agent/extract` | **Raw text + schema → typed JSON, grammar-constrained locally** |

`/status` is the one to call first when something is not working. `/requests` is the
one that answers questions.

## The agent endpoints

These are the two endpoints written for a calling AI rather than a person.

### `POST /agent/act` — act, then show the evidence

```bash
curl -s -X POST localhost:8201/agent/act \
  -H 'content-type: application/json' \
  -d '{"instruction":"Click Save and show me the API call","url_filter":"api"}'
```

The instruction goes to a local Needle 3 model, which picks one of three actions —
`click_element`, `eval_js`, `read_cookies` — and the Go side executes it on the
attached tab. The event buffer is **drained immediately before the action**, so
`captured_requests` contains what that action caused and not the page's background
chatter.

```json
{
  "action_executed": "click_element",
  "target": "Save",
  "captured_requests": [
    {"method": "POST", "url": "https://example.com/api/v1/update", "body": "{\"id\":123}", "has_body": true}
  ],
  "actions": [ ... every action, in the order the model gave them ... ],
  "reasoning": "...", "confidence": 0.91,
  "timing_ms": {"engine_load": 0, "model": 182, "action": 9, "capture": 141, "total": 337}
}
```

Three things worth knowing:

- **The capture polls.** A click and the request it causes are not simultaneous, so
  a single drain would answer "nothing happened" for a page about to send exactly
  the request you asked about. It polls until something matches, then for a short
  grace window to collect siblings. `settle_ms` overrides the window (default 400,
  capped at 5000); the request that arrives fast returns fast.
- **`read_cookies` returns names, never values**, matching `/cookies`. This
  endpoint is reached by an automated caller that never asked for a credential.
- **Routing is not perfect.** Measured on this machine, the model picks the
  intended tool **5/7** times on realistic instructions. Its failure mode is
  consistent: a bare domain name in the instruction pulls it towards
  `read_cookies` when `eval_js` was meant ("Read document.title from the page").
  The response always echoes `action_executed` and `target`, so a caller can
  detect the mis-route and either restate the instruction or bypass the model with
  `/eval` and `/click` directly. Dropping `read_cookies` from the list measures
  6/7 — cookies are already available via `GET /cookies`.

### `POST /agent/extract` — text in, typed JSON out

```bash
curl -s -X POST localhost:8201/agent/extract \
  -H 'content-type: application/json' \
  -d '{"text":"Order 88213 placed on 2024-03-11 by Ravi Sharma (ravi@example.com) for INR 4299. Status: shipped.",
       "schema":{"type":"object","properties":{
         "order_id":{"type":"string"},"customer_name":{"type":"string"},
         "customer_email":{"type":"string"},"total":{"type":"string"},
         "status":{"type":"string","enum":["pending","shipped","delivered","cancelled"]}},
        "required":["order_id","customer_name","status"]}}'
```

```json
{"extracted":{"customer_email":"ravi@example.com","customer_name":"Ravi Sharma",
  "order_id":"88213","status":"shipped","total":"4299"},
 "matched":true,"confidence":0.43,
 "timing_ms":{"engine_load":45,"model":204,"total":249}}
```

No browser is involved — the common case is an agent that already holds a payload
and needs it as fields, and requiring a live tab to parse text already in memory
would make the endpoint useless exactly when it is most wanted.

The schema is compiled into the decode grammar, so the answer conforms to it or
the model declines (`matched: false`, still a 200 — refusing to invent fields the
text does not contain is the grammar working, not a caller mistake).

**What the guarantee covers, precisely:** the output *parses* and conforms to the
schema. It does not make the values true. On clean prose this is exact — the
example above is a real response. On a deeply nested, escaped payload (a
`f.req`-style batch call) the same endpoint returned correctly *shaped* JSON with
**wrong values**, because a 121M model mis-read the escaping. Treat extraction as a
fast, offline, zero-cost first pass; verify against the raw payload when the fields
matter.


## Finding out what the app sends

This is the loop that matters, and it costs nothing — no generation, no credits.

```bash
# 1. Is anything attached, and what is it?
curl -s localhost:8201/status | jq

# 2. Drive the UI.
curl -s -X POST localhost:8201/click \
  -H 'content-type: application/json' \
  -d '{"text":"New project"}'

# 3. Read what that did.
curl -s 'localhost:8201/requests?filter=batchexecute' | jq '.requests[] | {method, url, body}'
```

Compare the body against what your client sends. Every argument position, every query
parameter, and every header the app includes is in there.

## What this found

Capturing the app's own requests is how these were settled — each had been guessed at
first, and each guess was wrong:

- **`SPrCad` takes the media id in `arg[0]`, not the content id.** The sibling
  generation calls take content ids, so the pattern was assumed to hold. It does not,
  and a wrong id comes back as a **null payload rather than an error**.
- **The source path for that call names the project and only the project.** The client
  was appending the editor route with the media id in it.
- **The app sends `f.sid` and a build label on every batchexecute request.** The client
  sent neither, and the build label it did send was a version behind.

None of the three is visible in a 200 response, and all three took a capture to find.

## Notes

- **Cookie values are opt-in.** `values=1` returns them; without it you get names and
  hosts. A debug tool that prints credentials by default turns every shared log into a
  leak.
- **Nothing attached returns 503, never an empty 200.** An empty list from `/requests`
  is indistinguishable from a page that made no requests, and that is exactly the
  confusion this tool exists to remove.
- **Bad arguments return 400 before the browser is consulted.** A missing `domain` is
  the caller's mistake, and answering it with "no extension attached" sends them to fix
  the wrong thing.
