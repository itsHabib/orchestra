package spawner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
)

type canonicalEnvSpec struct {
	Packages   PackageSpec `json:"packages"`
	Networking NetworkSpec `json:"networking"`
}

func envSpecHash(spec *EnvSpec) (string, error) {
	return hashCanonical(canonicalEnvSpec{
		Packages:   canonicalPackages(&spec.Packages),
		Networking: canonicalNetwork(spec.Networking),
	})
}

func hashCanonical(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("spawner: canonical hash marshal: %w", err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func canonicalPackages(packages *PackageSpec) PackageSpec {
	return PackageSpec{
		APT:   append([]string(nil), packages.APT...),
		Cargo: append([]string(nil), packages.Cargo...),
		Gem:   append([]string(nil), packages.Gem...),
		Go:    append([]string(nil), packages.Go...),
		NPM:   append([]string(nil), packages.NPM...),
		Pip:   append([]string(nil), packages.Pip...),
	}
}

func canonicalNetwork(network NetworkSpec) NetworkSpec {
	return NetworkSpec{
		Type:                 network.Type,
		AllowedHosts:         append([]string(nil), network.AllowedHosts...),
		AllowMCPServers:      network.AllowMCPServers,
		AllowPackageManagers: network.AllowPackageManagers,
	}
}

func hashFromMAEnv(env *anthropic.BetaEnvironment) (string, error) {
	if env == nil {
		return "", errors.New("spawner: nil managed environment")
	}
	spec := EnvSpec{
		Packages: PackageSpec{
			APT:   append([]string(nil), env.Config.Packages.Apt...),
			Cargo: append([]string(nil), env.Config.Packages.Cargo...),
			Gem:   append([]string(nil), env.Config.Packages.Gem...),
			Go:    append([]string(nil), env.Config.Packages.Go...),
			NPM:   append([]string(nil), env.Config.Packages.Npm...),
			Pip:   append([]string(nil), env.Config.Packages.Pip...),
		},
		Networking: NetworkSpec{
			Type:                 env.Config.Networking.Type,
			AllowedHosts:         append([]string(nil), env.Config.Networking.AllowedHosts...),
			AllowMCPServers:      env.Config.Networking.AllowMCPServers,
			AllowPackageManagers: env.Config.Networking.AllowPackageManagers,
		},
	}
	return envSpecHash(&spec)
}
