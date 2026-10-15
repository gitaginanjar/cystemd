// The files under fork/ are not a buildable module: they are overlaid onto
// the upstream Nomad tree by scripts/fork-assemble.sh. This go.mod only keeps
// them out of the root module's ./... package pattern.
module github.com/gitaginanjar/cystemd/fork

go 1.26.7
