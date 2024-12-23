# k8s-resource-wathcer

This tool subscribes to the objects in the cluster specified in the config and monitors changes in the specified fields
It was written for fun, just to deal with the dynamic client.

## How to build

```bash
go build .
```

## How to run

```bash
# with custom config
k8s-resource-watcher -config xxx.yaml
# jq
k8s-resource-watcher | jq .obj -c
# yq
k8s-resource-watcher | yq -p json -P .obj
```
