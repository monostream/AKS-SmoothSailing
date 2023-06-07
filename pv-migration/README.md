# Storage Migration Script

**Important: This migration script ensures that all data is retained during the migration process. There is no data loss when using this script.**

This repository contains a Go script that facilitates the migration of PersistentVolumes (PVs) from one storage class to another in a Kubernetes cluster. The script automates the process of updating PVs and their corresponding PersistentVolumeClaims (PVCs) to use a new storage class.

## Deprecation of Failure Domain Labels

The labels `failure-domain.beta.kubernetes.io/zone` and `failure-domain.beta.kubernetes.io/region` have been deprecated in AKS starting from version 1.24 and have been completely removed in version 1.26. Therefore, it is necessary to migrate your PVs and PVCs before updating to AKS version 1.26.

## Prerequisites

Before using this script, ensure that you have the following:

- Docker installed.
- Access to a Kubernetes cluster.
- The necessary permissions to create, update, and delete PVs and PVCs in the cluster.

## Usage

To run the script using Docker, follow these steps:

1. Clone the repository and build the dockerfile:

   ```bash
   git clone https://github.com/monostream/AKS-SmoothSailing.git
   cd pv-migration
   docker build -t pv-migration .
   ```

2. Run the Docker container:

   a) execute the command directly and mounting the local kubeconfig file
   ```bash
   docker run -v ~/.kube/config:/app/.kube/config pv-migration --existing-storageclass <existing-storage-class> --new-storageclass <new-storage-class> --clustername <cluster-name>
   ```

   Replace `<existing-storage-class>` with the name of the existing storage class you want to migrate from, `<new-storage-class>` with the name of the new storage class you want to migrate to, and `<cluster-name>` with the name of your Kubernetes cluster.

   Note: The script requires a valid kubeconfig file to connect to the Kubernetes cluster. By mounting `~/.kube/config` to `/app/.kube/config` in the container, the script can access the kubeconfig file.


   b) If you prefer to interact interactive way, you can start the image like this. In the Image AZ-CLI and kubectl is available

   ```bash
   docker run -it --rm -v ~/.kube:/root/.kube pv-migration:latest bash
   az login
   az aks get-credentials --admin --resource-group your-rg --name your-cluster

   pv-migration --existing-storageclass <existing-storage-class> --new-storageclass <new-storage-class> --clustername <cluster-name>
   ```

3. Follow the script's output to monitor the migration process. The script will update the PVs and PVCs accordingly.

## Build the image by your self

   ```bash
   git clone https://github.com/monostream/AKS-SmoothSailing.git
   cd pv-migration
   go mod download
   go build
   ```


## Flags

The script accepts the following flags:

- `--existing-storageclass`: The name of the existing storage class (required).
- `--new-storageclass`: The name of the new storage class to be used (required).
- `--clustername`: The name of the Kubernetes cluster (required).
- `--delete-migrated`: (Optional) If provided, the script will delete the migrated PVs after the migration is complete.

## Notes

- The script uses the Azure Disk CSI driver and is specifically designed for migrating PVs provisioned by the "azure-disk-dynamic-provisioner" to another storage class.
- The script assumes that the PVs to be migrated have the annotation "volumehelper.VolumeDynamicallyCreatedByKey" set to "azure-disk-dynamic-provisioner".
- The script scales down the necessary controllers (DaemonSets, StatefulSets, Deployments, and ReplicaSets) to 0 replicas before updating the PVs and PVCs. It restores the original replica counts after the migration is complete.
- The script creates new PV manifests for the migrated PVs in the format `clustername/pvName-csi.json` and saves them to the local filesystem.
- It also creates new PVC manifests for the corresponding PVCs in the format `clustername/new-pvc.claimRefName.cl

aimRefNamespace.json` and saves them to the local filesystem.
- The original PV manifests and PVC manifests are saved as backups in the format `clustername/pvName.json` and `clustername/original-pvc.claimRefName.claimRefNamespace.json`, respectively.
- The script updates the PVs and PVCs in the cluster and prints relevant logs during the process.