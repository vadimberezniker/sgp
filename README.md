This tool exists to make language servers like gopls be able to process generated protos for Bazel workspaces.

It does this by creating symlinks to the generated protos in locations where the language compiler expects them.

To use the tool run `go run main.go --dirs /path/to/bazel/workspace`

Don't forget to build the proto targets using Bazel as the symlinks point to the generated protos in the bazel-bin directory.
