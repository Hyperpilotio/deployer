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

API Server
-----------
  - Deployment state (Created, Status)
  - Call Deployer for cluster manager specific actions

AWS
-----------
  - Handles launching EC2 servers for running a cluster manager

GCP
-----------
  - Handles launching GKE servers for running a cluster manager  
      * Create your GCP projectId first

Clustermanagers
-----------
  - Handles all cluster manager specific logic
