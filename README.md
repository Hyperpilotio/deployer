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
      * Install gcloud
      * Create your GCP projectId first (Use GCP web console or gcloud)
      * System will auto create 'compute Engine default service account' when you first click 'Container Engine' from GCP web console
      * Enable GAE service support because we need to use 'Storage' to upload serviceAccount JSON file  
      * Run deployer/build_gcp_serviceAccoutFile.sh to gen your serviceAccount JSON file (You can also change iam-account)
      * Setting gcpServiceAccountJSONFile path to dev.config (See template.config)
      * Write deploy-gcp.json to deploy

Clustermanagers
-----------
  - Handles all cluster manager specific logic
