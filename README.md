# ABMeter Go SDK

Go server SDK for [ABMeter](https://abmeter.ai) — feature flags and A/B
experiments with client-side evaluation. Zero dependencies outside the Go
standard library.

Assignment is random across users and consistent per user: the SDK fetches
your assignment configuration, buckets each user locally, and reports
exposures and events to the platform in the background.

## Install

```bash
go get github.com/abmeter/abmeter-go
```

## Quick start

Parameters and event types must already exist on the platform (create them
via the ABMeter API or MCP — there is no Web UI for this). The API key comes
from your ABMeter workspace.

```go
package main

import (
	"context"
	"fmt"

	abmeter "github.com/abmeter/abmeter-go"
)

func main() {
	err := abmeter.Configure(abmeter.Config{
		APIKey: "your-api-key",
	})
	if err != nil {
		panic(err)
	}
	// Flush queued exposures/events before exit.
	defer abmeter.Reset(context.Background())

	user := abmeter.User{UserID: "user-123"}

	// Resolve a parameter: default value, feature-flag override, or the
	// user's experiment variant — the SDK records the exposure for you.
	color, err := abmeter.ResolveParameter(user, "button-color")
	if err != nil {
		color = "blue" // fall back to your own default on errors
	}
	fmt.Println("button color:", color)

	// Track a conversion event.
	abmeter.TrackEvent("purchase", user.UserID, map[string]any{"price": 4.99})
}
```

## Configuration

```go
abmeter.Configure(abmeter.Config{
	APIKey:        "your-api-key",
	BaseURL:       "https://abmeter.ai",  // default
	FlushInterval: 60 * time.Second,      // background submit cadence (default)
	FetchInterval: 60 * time.Second,      // config re-check cadence (default)
	Logger:        log.Default(),         // submitter errors; default stderr
})
```

The assignment configuration is polled with `If-None-Match`, so an unchanged
configuration costs a `304 Not Modified` — cheap to poll frequently.

### Offline mode

For tests, pass a static assignment configuration instead of an API key:
nothing is fetched and nothing is submitted.

```go
abmeter.Configure(abmeter.Config{StaticConfig: configJSON})
```

## Users

`UserID` is the only identity the platform needs — it is the randomization
unit. `Email` is optional and only consulted by email-predicate audiences; a
missing email simply never matches them.

```go
abmeter.User{UserID: "user-123", Email: "user@example.com"}
```

## Shutdown

Exposures and events are queued and submitted in the background. Before the
process exits, drain the queue:

```go
// Graceful: flushes everything, bounded by the context (default 5s).
err := abmeter.Reset(ctx)

// Immediate: discards anything queued, no network I/O.
abmeter.ResetNow()
```

`Reset` returns an error when the drain did not finish in time.

## Errors

All entry points return errors instead of panicking. A failing config refresh
never takes resolution down: the SDK keeps serving the last-known
configuration until a refresh succeeds.

## License

MIT — see [LICENSE.txt](./LICENSE.txt).
