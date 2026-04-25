package spawner

import (
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

func toEnvCreateParams(spec *EnvSpec, key string) anthropic.BetaEnvironmentNewParams {
	return anthropic.BetaEnvironmentNewParams{
		Name:     key,
		Config:   envConfigParams(spec),
		Metadata: envMetadata(spec),
	}
}

func envMetadata(spec *EnvSpec) map[string]string {
	md := cloneStringMap(spec.Metadata)
	md[orchestraMetadataProject] = spec.Project
	md[orchestraMetadataEnv] = spec.Name
	md[orchestraMetadataVersion] = orchestraVersionV2
	return md
}

func envConfigParams(spec *EnvSpec) anthropic.BetaCloudConfigParams {
	return anthropic.BetaCloudConfigParams{
		Packages:   packageParams(&spec.Packages),
		Networking: networkParams(spec.Networking),
	}
}

func packageParams(packages *PackageSpec) anthropic.BetaPackagesParams {
	return anthropic.BetaPackagesParams{
		Type:  anthropic.BetaPackagesParamsTypePackages,
		Apt:   append([]string(nil), packages.APT...),
		Cargo: append([]string(nil), packages.Cargo...),
		Gem:   append([]string(nil), packages.Gem...),
		Go:    append([]string(nil), packages.Go...),
		Npm:   append([]string(nil), packages.NPM...),
		Pip:   append([]string(nil), packages.Pip...),
	}
}

func networkParams(network NetworkSpec) anthropic.BetaCloudConfigParamsNetworkingUnion {
	if strings.EqualFold(network.Type, "unrestricted") || strings.EqualFold(network.Type, "open") {
		unrestricted := anthropic.NewBetaUnrestrictedNetworkParam()
		return anthropic.BetaCloudConfigParamsNetworkingUnion{OfUnrestricted: &unrestricted}
	}
	return anthropic.BetaCloudConfigParamsNetworkingUnion{
		OfLimited: &anthropic.BetaLimitedNetworkParams{
			AllowedHosts:         append([]string(nil), network.AllowedHosts...),
			AllowMCPServers:      anthropic.Bool(network.AllowMCPServers),
			AllowPackageManagers: anthropic.Bool(network.AllowPackageManagers),
		},
	}
}
