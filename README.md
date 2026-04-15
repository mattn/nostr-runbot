# nostr-runbot

A Nostr bot that compiles and runs code snippets via [paiza.io](https://paiza.io/) and replies with the output.

## Usage

Post a note containing a `/run` command and mention the bot.

````
/run go
package main

import "fmt"

func main() {
    fmt.Println("hello, nostr")
}
````

The first line is `/run <lang>` and the rest is the source code. The bot replies with the program's output.

To see the list of supported languages, post:

```
/run list
```

To re-execute a previous `/run`, reply to the bot's output with:

```
/rerun
```

The bot looks up the thread's root event (the original `/run`) from relays and re-runs it.

Both text notes (kind 1) and channel messages (kind 42) are supported. Replies are returned in the same kind, and for channel messages the root `e` tag is preserved so the reply stays in the same channel.

### Supported languages

`rb`/`ruby`, `py`/`python`, `go`, `js`/`node`, `c`, `cpp`/`c++`, `rs`/`rust`, `sh`/`bash`, `php`, `pl`/`perl`, `swift`

See `langToCompiler` in `main.go` for the exact paiza.io language names.

## Running

`nostr-runbot` is an HTTP server that accepts a signed Nostr event as a POST body and returns a signed reply event. It is meant to be invoked by a front-end that forwards Nostr events over HTTP.

```
$ export BOT_NSEC=nsec1xxxxxxxxxxx...
$ ./nostr-runbot
```

Environment variables:

- `BOT_NSEC` (required): the bot's private key in `nsec` form.
- `ADDR` (optional): listen address, default `:8080`.
- `RELAYS` (optional): comma-separated relay URLs used by `/rerun` to fetch the original event. Defaults to a few Japanese relays plus `relay.damus.io`.

## Installation

```
go install github.com/mattn/nostr-runbot@latest
```

Or with Docker:

```
docker pull ghcr.io/mattn/nostr-runbot:latest
```

## License

MIT

## Author

Yasuhiro Matsumoto (a.k.a. mattn)
