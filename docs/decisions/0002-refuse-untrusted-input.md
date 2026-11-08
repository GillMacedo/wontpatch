# 0002: Exit 2 on any input it can't trust

**Decision:** if either side is empty, isn't valid YAML or JSON, holds something other than Kubernetes objects, looks cut off, is a chart or kustomization, defines an object twice, or is a live object it can't read, wontpatch exits 2 and prints only the reason. `--fail-on none` doesn't change this.

**Why:** the usual way to call it is on the output of `helm template` or `kustomize build`. When those fail they print an error or nothing, and a checker that then reports "no immutable-field changes" turns a broken render into a green build. A file cut mid-line fails the final newline check.

**Limit:** a file cut on a line break can still parse and can't always be told from an edited file. Those cases are warnings, not refusals: removed objects, new objects after the end of OLD's file, and a last object that ends on a key with no value or stops before its counterpart. They fail under the default `--fail-on warn`. An earlier version refused the last case with exit 2, which also refused ordinary edits at the end of a file, so it became a warning.
