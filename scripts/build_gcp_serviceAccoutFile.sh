#!/bin/bash

echo "Building GCP Service account JSON File"
gcloud iam service-accounts keys create \
    ~/gcpServiceAccount.json \
    --iam-account $(gcloud iam service-accounts list | grep "Compute Engine default service account" | awk '{print $6}')

