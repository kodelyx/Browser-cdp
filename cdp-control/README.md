# cdp-control

Drives a browser through the generic CDP extension, on its own ports.

It exists because debugging has to be separate from the thing being debugged.
Finding out what an app sends means driving its UI and reading its own network
traffic, and doing that from inside the product means every experiment runs against
the product's bridge, its scope, and its state. This is the same extension with
nothing else attached.

## Run it

```bash
go build -o cdp-control .
./cdp-control                       # ws 9222, http 8201
./cdp-control -ws 127.0.0.1:9223    # when the Flow engine also holds 9222
./cdp-control -http 127.0.0.1:8301  # when 8201 is taken
```

The default WebSocket port is 9222 because that is the extension's own default, so
nothing in the browser needs configuring. The Flow engine also uses 9222 — run one at
a time, or move this one with `-ws`.

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

`/status` is the one to call first when something is not working. `/requests` is the
one that answers questions.

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
