// Command arrivals checks the provider side rate limit of a load run. It reads the mock
// provider arrival log and fails when any channel has a 1s window above -max-window or an
// average above -max-avg per second. Windows are sliding: the count of arrivals in
// [t, t+1s) for every arrival t.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"time"
)

type arrival struct {
	Channel string `json:"channel"`
	At      string `json:"at"`
}

func main() {
	url := flag.String("url", "http://localhost:9090/_control/arrivals", "arrivals endpoint")
	maxWindow := flag.Int("max-window", 105, "largest allowed arrivals in any 1s window")
	maxAvg := flag.Float64("max-avg", 100, "largest allowed average arrivals per second")
	flag.Parse()
	if err := run(*url, *maxWindow, *maxAvg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(url string, maxWindow int, maxAvg float64) error {
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Arrivals []arrival `json:"arrivals"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return err
	}
	byChannel := map[string][]time.Time{}
	for _, a := range body.Arrivals {
		t, err := time.Parse(time.RFC3339Nano, a.At)
		if err != nil {
			return err
		}
		byChannel[a.Channel] = append(byChannel[a.Channel], t)
	}
	failed := false
	for _, ch := range []string{"sms", "email", "push"} {
		ts := byChannel[ch]
		if len(ts) == 0 {
			fmt.Printf("%-5s arrivals=0\n", ch)
			continue
		}
		sort.Slice(ts, func(i, j int) bool { return ts[i].Before(ts[j]) })
		maxW, j := 0, 0
		for i := range ts {
			for ts[i].Sub(ts[j]) >= time.Second {
				j++
			}
			if n := i - j + 1; n > maxW {
				maxW = n
			}
		}
		span := ts[len(ts)-1].Sub(ts[0]).Seconds()
		avg := float64(len(ts))
		if span > 1 {
			avg = float64(len(ts)) / span
		}
		verdict := "ok"
		if maxW > maxWindow || avg > maxAvg {
			verdict = "FAIL"
			failed = true
		}
		fmt.Printf("%-5s arrivals=%d max_1s_window=%d avg_per_s=%.1f span_s=%.1f %s\n", ch, len(ts), maxW, avg, span, verdict)
	}
	if failed {
		return fmt.Errorf("rate limit check failed (max window %d, max avg %.0f/s)", maxWindow, maxAvg)
	}
	return nil
}
