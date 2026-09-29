package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"
)

const feedFetchCmdTimeout = 60 * time.Second

func runFeedFetch(args []string) int {
	fs := flag.NewFlagSet("feed-fetch", flag.ContinueOnError)
	socket := fs.String("socket", "", "Absolute path of the feed service socket")
	key := fs.Int64("key", 0, "Deadline key in Unix seconds (default: the feed's last sealed key)")
	out := fs.String("out", "", "File to write the seal bytes to (default: stdout)")
	describe := fs.Bool("describe", false, "Print the feed's describe reply as JSON and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "feed-fetch: unexpected arguments %v\n", fs.Args())
		return 2
	}
	if errs := feedSocketPathErrors("--socket", *socket); len(errs) > 0 {
		fmt.Fprintf(os.Stderr, "feed-fetch: %s\n", errs[0])
		return 2
	}
	client := newSharedFeedClientWithEndpoints([]sharedFeedEndpoint{{Name: "feed", Socket: *socket}})
	ctx, cancel := context.WithTimeout(context.Background(), feedFetchCmdTimeout)
	defer cancel()

	if *describe || *key == 0 {
		d := client.Describe(ctx)[0]
		if d.Err != nil {
			fmt.Fprintf(os.Stderr, "feed-fetch: describe %s: %v\n", *socket, d.Err)
			return 1
		}
		if *describe {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(d.Header); err != nil {
				fmt.Fprintf(os.Stderr, "feed-fetch: %v\n", err)
				return 1
			}
			return 0
		}
		if d.Describe.LastSealKey == 0 {
			fmt.Fprintf(os.Stderr, "feed-fetch: %s has no sealed key yet\n", *socket)
			return 1
		}
		*key = d.Describe.LastSealKey
	}

	ep := sharedFeedEndpoint{Name: "feed", Socket: *socket}
	doc, h, err := client.fetchFrom(ctx, ep, *key, client.giveUpAt(*key))
	if err != nil {
		fmt.Fprintf(os.Stderr, "feed-fetch: key %d from %s: %v\n", *key, *socket, err)
		return 1
	}
	h2, body, err := client.roundTrip(ctx, *socket, feedWireRequest{V: feedWireVersion, Op: feedWireOpSnapshot, Key: *key})
	if err != nil || h2.Status != feedWireStatusSealed || h2.Hash != h.Hash || feedSealHash(body) != h.Hash {
		fmt.Fprintf(os.Stderr, "feed-fetch: key %d changed or vanished between two reads of %s (%v)\n", *key, *socket, err)
		return 1
	}
	if *out == "" {
		if _, err := os.Stdout.Write(body); err != nil {
			fmt.Fprintf(os.Stderr, "feed-fetch: %v\n", err)
			return 1
		}
	} else if err := os.WriteFile(*out, body, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "feed-fetch: write %s: %v\n", *out, err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "feed-fetch: key=%d status=sealed source=%s instance=%s generation=%d hash=%s bytes=%d keys=%d mids=%d\n",
		*key, doc.Source, doc.Instance, doc.Generation, h.Hash, len(body), len(doc.Keys), len(doc.Mids))
	return 0
}
