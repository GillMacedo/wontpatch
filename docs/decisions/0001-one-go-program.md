# 0001: One Go program with the YAML parser copied in

**Decision:** all logic lives in `plugin/scripts`, and go.yaml.in/yaml/v2 is copied into it as `yaml_*.go` with the package renamed to `main`, instead of being a module dependency.

**Why:** Claude Code installs only the `plugin/` directory, with no `go.mod`, so the skill has to build from the `.go` files alone, and Go has no YAML parser in the standard library. The copied parser is the one kubectl uses through `sigs.k8s.io/yaml`, so YAML 1.1 details like `yes` and `on` parse the same way. `tests/plugin_test.go` builds a copy of `plugin/` outside the module with the skill's own command.

**Alternatives:** a nested module with a vendor directory, which would need its own `plugin/scripts/v0.1.0` tags and break `go run ...@v0.1.0`; an MCP server shipped through npm, which is more than a file comparison needs; and a small hand-written YAML parser, rejected as too easy to get wrong.
