# Browser-cdp

Two things that go together: a Chrome extension that exposes the DevTools Protocol
over a local socket, and a Go tool that uses it to find out what a web app actually
does.

```
extension/      the Chrome extension — attach to a tab, send CDP commands,
                stream events, read cookies
cdp-control/    a Go tool that drives the extension, and reports what it sees
```

They are independent. The extension is useful on its own to anything that speaks its
protocol; the tool is useful on its own as a debugging surface.

## Why the extension

It is a drop-in alternative to launching Chrome with `--remote-debugging-port=9222`,
with two advantages: the browser stays your normal signed-in browser — no separate
profile, no re-login — and scoping is available when you want it, through
`targetUrlPrefixes`, `cookieDomains` and `blockedMethods`. With empty allowlists the
backend gets full raw-CDP access to every tab, exactly like the debugging port.

It has no knowledge of any particular website.

→ [`extension/README.md`](extension/README.md)

## Why the tool

Finding out what an app sends means driving its UI and reading its own network
traffic. Doing that from inside the product means every experiment runs against the
product's bridge, its scope and its state — which is how a debugging session turns
into an afternoon of guessing.

`cdp-control` is the same extension with nothing else attached:

```bash
cd cdp-control && go build -o cdp-control . && ./cdp-control
```

```
ws   127.0.0.1:9222   the extension dials in (its own default)
http 127.0.0.1:8201   /status /requests /events /tabs /eval /click /cdp /cookies
```

Load `extension/` as an unpacked extension in Chrome. It dials out; there is nothing
to point at it.

→ [`cdp-control/README.md`](cdp-control/README.md)

## The loop

This is what the pair is for, and it costs nothing — no generation, no API calls.

```bash
curl -s localhost:8201/status | jq                       # what is attached
curl -s -X POST localhost:8201/click \
  -H 'content-type: application/json' -d '{"text":"New project"}'
curl -s 'localhost:8201/requests?filter=batchexecute' | jq '.requests[] | {method, url, body}'
```

`/requests` parses the buffered network events into `{method, url, body}`. Compare
that against what your client sends: every argument position, every query parameter
and every header is in there.

## What this found

Three things, each of which had been guessed at first and each of which was wrong.
None is visible in a successful response.

- **One call takes a media id where its siblings take a content id.** The obvious
  pattern was assumed to hold; it does not, and the wrong id comes back as a **null
  payload rather than an error**.
- **A source path named an editor route the app does not send.** The media id travels
  in the body instead.
- **The app sends a session id and a build label on every request.** The client sent
  neither, and the label it did send was a version behind.

All three took a capture to find, and none of them would have taken an afternoon if
the capture had come first.
