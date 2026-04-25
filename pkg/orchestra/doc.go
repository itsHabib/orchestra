// Package orchestra is the experimental Go SDK for the orchestra workflow
// engine. The surface is unstable; expect breaking changes without semver
// signaling until the surface is marked stable in a later release. See
// CHANGELOG.md for the per-release surface change log.
//
// Typical use:
//
//	cfg, warnings, err := orchestra.LoadConfig("orchestra.yaml")
//	for _, w := range warnings {
//	    fmt.Fprintln(os.Stderr, w)
//	}
//	if err != nil {
//	    return err
//	}
//	res, err := orchestra.Run(ctx, cfg)
//	if err != nil {
//	    return err
//	}
//	for name, team := range res.Teams {
//	    fmt.Printf("%s: %s (%d turns, %.2f USD)\n",
//	        name, team.Status, team.NumTurns, team.CostUSD)
//	}
//
// Experimental: this package is unstable and may change without notice.
package orchestra
