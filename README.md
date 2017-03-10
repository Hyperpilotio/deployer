# Deployer

# Development

* Required
    * Unix-like environment

```{shell}
# init
make init

# test
make test

# build
make build

# for developer
make dev-test
```

# Use 
```{shell}
cd $GOPATH/src/github.com/hyperpilotio/deployer
make init 
go build 
./deployer -v 1 --config documents/template.config
```
