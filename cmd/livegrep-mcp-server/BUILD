load("@rules_go//go:def.bzl", "go_binary", "go_library")

go_library(
    name = "go_default_library",
    srcs = ["main.go"],
    importpath = "github.com/livegrep/livegrep/cmd/livegrep-mcp-server",
    visibility = ["//visibility:private"],
    deps = [
        "//src/proto:go_proto",
        "@org_golang_google_grpc//:go_default_library",
        "@org_golang_google_grpc//credentials/insecure:go_default_library",
    ],
)

go_binary(
    name = "livegrep-mcp-server",
    embed = [":go_default_library"],
    visibility = ["//visibility:public"],
)
