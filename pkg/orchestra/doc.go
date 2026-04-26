// Package orchestra is the experimental Go SDK for the orchestra workflow
// engine. The surface is unstable; expect breaking changes without semver
// signaling until the surface is marked stable in a later release. See
// CHANGELOG.md for the per-release surface change log.
//
// One-shot blocking use:
//
//	res, err := orchestra.LoadConfig("orchestra.yaml")
//	if err != nil {
//	    return err // I/O or parse failure
//	}
//	for _, w := range res.Warnings {
//	    fmt.Fprintln(os.Stderr, w)
//	}
//	if !res.Valid() {
//	    return res.Err()
//	}
//	out, err := orchestra.Run(ctx, res.Config)
//	if err != nil {
//	    return err
//	}
//	for name, team := range out.Teams {
//	    fmt.Printf("%s: %s (%d turns, %.2f USD)\n",
//	        name, team.Status, team.NumTurns, team.CostUSD)
//	}
//
// Asynchronous, observable use:
//
//	h, err := orchestra.Start(ctx, cfg)
//	if err != nil {
//	    return err
//	}
//	go func() {
//	    for ev := range h.Events() {
//	        orchestra.PrintEvent(os.Stdout, ev)
//	    }
//	}()
//	res, err := h.Wait()
//
// To get the CLI's colored console output without managing a goroutine,
// pass [WithEventHandler] to [Run]:
//
//	res, err := orchestra.Run(ctx, cfg,
//	    orchestra.WithEventHandler(func(ev orchestra.Event) {
//	        orchestra.PrintEvent(os.Stdout, ev)
//	    }),
//	)
//
// Experimental: this package is unstable and may change without notice.
package orchestra
