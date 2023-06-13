package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/go-autorest/autorest/to"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

type controllerRef struct{}

func main() {
	migrateStorage()

	log.Println("done")
}

func migrateStorage() {
	var existingStorageClass string
	var newStorageClass string
	var clustername string
	var deletePVs bool

	flag.StringVar(&existingStorageClass, "existing-storageclass", "", "The name of the existing storage class")
	flag.StringVar(&newStorageClass, "new-storageclass", "", "The name of the new storage class to be used")
	flag.StringVar(&clustername, "clustername", "", "The name of the Kubernetes cluster")
	flag.BoolVar(&deletePVs, "delete-migrated", false, "If the script should delete the migrated PVs after the migration is complete")

	var kubeconfig *string

	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}

	flag.Parse()

	if existingStorageClass == "" || newStorageClass == "" || clustername == "" {
		log.Println("error: please provide all the required flags: --existing-storageclass, --new-storageclass, --clustername")
		os.Exit(1)
	}

	if _, err := os.Stat(*kubeconfig); err != nil {
		if os.IsNotExist(err) {
			log.Printf("error: the provided kubeconfig file does not exist: %v\n", kubeconfig)
		} else {
			log.Printf("error reading the kubeconfig file: %v\n", err)
		}

		os.Exit(1)
	}

	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)

	if err != nil {
		panic(err.Error())
	}

	client, err := kubernetes.NewForConfig(config)

	if err != nil {
		panic(err.Error())
	}

	if err := os.Mkdir(clustername, 0755); err != nil {
		if !os.IsExist(err) {
			panic(err)
		}
	}

	ctx := context.Background()

	newStorageClassObj, err := client.StorageV1().StorageClasses().Get(ctx, newStorageClass, metav1.GetOptions{})

	if err != nil {
		panic(err)
	}

	skuName := newStorageClassObj.Parameters["skuname"]

	if skuName == "" {
		panic("error: the new storage class does not have a sku name")
	}

	controllerRefs := make(map[string]controllerRef)

	pvs, err := client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})

	if err != nil {
		panic(err)
	}

	for _, pv := range pvs.Items {
		// Check if the PV is provisioned by "azure-disk-dynamic-provisioner"
		if v, ok := pv.Annotations["volumehelper.VolumeDynamicallyCreatedByKey"]; !ok || v != "azure-disk-dynamic-provisioner" {
			continue
		}

		pvName := pv.Name
		claimRefNamespace := pv.Spec.ClaimRef.Namespace
		claimRefName := pv.Spec.ClaimRef.Name
		persistentVolumeReclaimPolicy := pv.Spec.PersistentVolumeReclaimPolicy
		storageClassName := pv.Spec.StorageClassName
		capacityStorage := pv.Spec.Capacity[corev1.ResourceStorage]
		azureDiskDiskURI := pv.Spec.PersistentVolumeSource.AzureDisk.DataDiskURI

		if !strings.EqualFold(storageClassName, existingStorageClass) {
			log.Println("wrong StorageClass", existingStorageClass, storageClassName)
			continue
		}

		if pvName == "" || claimRefNamespace == "" || claimRefName == "" || persistentVolumeReclaimPolicy == "" || storageClassName == "" || capacityStorage.String() == "" || azureDiskDiskURI == "" {
			log.Println("error: one of the required fields of the pv is empty, pv: " + pvName)
			continue
		}

		if persistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
			log.Printf("updating ReclaimPolicy for %s to Retain\n", pvName)

			pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimRetain

			if _, err = client.CoreV1().PersistentVolumes().Update(context.Background(), &pv, metav1.UpdateOptions{}); err != nil {
				log.Println("error: failed to update ReclaimPolicy for " + pvName + " to retain policy: " + err.Error())
				continue
			}

			if deletePVs {
				defer func() {
					if err = client.CoreV1().PersistentVolumes().Delete(context.Background(), pvName, metav1.DeleteOptions{}); err != nil {
						log.Println("error: deleting old pv " + pvName + ": " + err.Error())
					}
				}()
			}
		}

		pods, err := client.CoreV1().Pods(claimRefNamespace).List(ctx, metav1.ListOptions{})

		if err != nil {
			log.Println("error: failed to list pods in namespace " + claimRefNamespace + err.Error())
			continue
		}

		for _, pod := range pods.Items {
			for _, volume := range pod.Spec.Volumes {
				if volume.PersistentVolumeClaim == nil || !strings.EqualFold(volume.PersistentVolumeClaim.ClaimName, claimRefName) {
					continue
				}

				if len(pod.ObjectMeta.OwnerReferences) == 0 {
					log.Println("error: pod does not have any owner references, pod: " + pod.Name + " in namespace " + pod.Namespace)
					continue
				}

				controllerKind := pod.ObjectMeta.OwnerReferences[0].Kind
				controllerName := pod.ObjectMeta.OwnerReferences[0].Name

				if controllerKind == "" || controllerName == "" {
					log.Println("error: one of the owner reference fields of the pod is empty, pod: " + pod.Name + " in namespace " + pod.Namespace)
					continue
				}

				controllerRefKey := fmt.Sprintf("%s/%s/%s", controllerKind, claimRefNamespace, controllerName)

				if _, ok := controllerRefs[controllerRefKey]; ok {
					continue
				}

				switch controllerKind {
				case "DaemonSet":
					daemonset, err := client.AppsV1().DaemonSets(claimRefNamespace).Get(ctx, controllerName, metav1.GetOptions{})

					if err != nil {
						log.Println("error: failed to get daemonset " + controllerName + " in namespace " + claimRefNamespace + ": " + err.Error())
						continue
					}

					daemonset.Spec.Template.Spec.NodeSelector["storage-migration"] = "in-progress"

					if _, err = client.AppsV1().DaemonSets(claimRefNamespace).Update(ctx, daemonset, metav1.UpdateOptions{}); err != nil {
						log.Println("error: failed to update daemonset " + controllerName + " in namespace " + claimRefNamespace + ": " + err.Error())
						continue
					}

					defer func() {
						daemonset, err := client.AppsV1().DaemonSets(claimRefNamespace).Get(ctx, controllerName, metav1.GetOptions{})

						if err != nil {
							log.Println("error: failed to get object for daemonset restore" + controllerName + " in namespace " + claimRefNamespace + ": " + err.Error())
						}

						delete(daemonset.Spec.Template.Spec.NodeSelector, "storage-migration")

						if _, err = client.AppsV1().DaemonSets(claimRefNamespace).Update(ctx, daemonset, metav1.UpdateOptions{}); err != nil {
							log.Println("error: failed to restore daemonset " + controllerName + " in namespace " + claimRefNamespace + ": " + err.Error())
						}
					}()

				case "StatefulSet":
					statefulset, err := client.AppsV1().StatefulSets(claimRefNamespace).Get(ctx, controllerName, metav1.GetOptions{})

					if err != nil {
						log.Println("error: failed to get statefulset " + controllerName + " in namespace " + claimRefNamespace + ": " + err.Error())
						continue
					}

					originalReplicas := statefulset.Spec.Replicas

					statefulset.Spec.Replicas = to.Int32Ptr(0)

					if _, err = client.AppsV1().StatefulSets(claimRefNamespace).Update(ctx, statefulset, metav1.UpdateOptions{}); err != nil {
						log.Println("error: failed to scale down statefulset " + controllerName + " in namespace " + claimRefNamespace + ": " + err.Error())
						continue
					}

					defer func() {
						statefulset, err := client.AppsV1().StatefulSets(claimRefNamespace).Get(ctx, controllerName, metav1.GetOptions{})

						if err != nil {
							log.Println("error: failed to get object for statefulset restore" + controllerName + " in namespace " + claimRefNamespace + ": " + err.Error())
						}

						statefulset.Spec.Replicas = originalReplicas

						if _, err = client.AppsV1().StatefulSets(claimRefNamespace).Update(ctx, statefulset, metav1.UpdateOptions{}); err != nil {
							log.Println("error: failed to restore statefulset " + controllerName + " in namespace " + claimRefNamespace + " to " + strconv.Itoa(int(to.Int32(originalReplicas))) + " replicas: " + err.Error())
						}
					}()

				case "ReplicaSet":
					replicaset, err := client.AppsV1().ReplicaSets(claimRefNamespace).Get(ctx, controllerName, metav1.GetOptions{})

					if err != nil {
						log.Println("error: failed to get replicaset " + controllerName + " in namespace " + claimRefNamespace + ": " + err.Error())
						continue
					}

					if len(replicaset.ObjectMeta.OwnerReferences) != 0 {
						controllerKind = replicaset.ObjectMeta.OwnerReferences[0].Kind
						controllerName = replicaset.ObjectMeta.OwnerReferences[0].Name

						if controllerKind == "" || controllerName == "" {
							log.Println("error: one of the owner reference fields of the replica set is empty, replicaset: " + replicaset.Name + " in namespace " + replicaset.Namespace)
							continue
						}

						deployment, err := client.AppsV1().Deployments(claimRefNamespace).Get(ctx, controllerName, metav1.GetOptions{})

						if err != nil {
							log.Println("error: failed to get deployment " + controllerName + " in namespace " + claimRefNamespace + ": " + err.Error())
							continue
						}

						originalReplicas := deployment.Spec.Replicas

						deployment.Spec.Replicas = to.Int32Ptr(0)

						if _, err = client.AppsV1().Deployments(claimRefNamespace).Update(ctx, deployment, metav1.UpdateOptions{}); err != nil {
							log.Println("error: failed to scale down deployment " + controllerName + " in namespace " + claimRefNamespace + ": " + err.Error())
							continue
						}

						defer func() {
							deployment, err := client.AppsV1().Deployments(claimRefNamespace).Get(ctx, controllerName, metav1.GetOptions{})

							if err != nil {
								log.Println("error: failed to get object for deployment restore" + controllerName + " in namespace " + claimRefNamespace + ": " + err.Error())
							}

							deployment.Spec.Replicas = originalReplicas

							if _, err = client.AppsV1().Deployments(claimRefNamespace).Update(ctx, deployment, metav1.UpdateOptions{}); err != nil {
								log.Println("error: failed to restore deployment " + controllerName + " in namespace " + claimRefNamespace + " to " + strconv.Itoa(int(to.Int32(originalReplicas))) + " replicas: " + err.Error())
							}
						}()
					} else {
						originalReplicas := replicaset.Spec.Replicas

						replicaset.Spec.Replicas = to.Int32Ptr(0)

						if _, err = client.AppsV1().ReplicaSets(claimRefNamespace).Update(ctx, replicaset, metav1.UpdateOptions{}); err != nil {
							log.Println("error: failed to scale down replicaset " + controllerName + " in namespace " + claimRefNamespace + ": " + err.Error())
							continue
						}

						defer func() {
							replicaset, err := client.AppsV1().ReplicaSets(claimRefNamespace).Get(ctx, controllerName, metav1.GetOptions{})

							if err != nil {
								log.Println("error: failed to get object for replicaset restore" + controllerName + " in namespace " + claimRefNamespace + ": " + err.Error())
							}

							replicaset.Spec.Replicas = originalReplicas

							if _, err = client.AppsV1().ReplicaSets(claimRefNamespace).Update(ctx, replicaset, metav1.UpdateOptions{}); err != nil {
								log.Println("error: failed to restore replicaset " + controllerName + " in namespace " + claimRefNamespace + " to " + strconv.Itoa(int(to.Int32(originalReplicas))) + " replicas: " + err.Error())
							}
						}()
					}

				case "Deployment":
					deployment, err := client.AppsV1().Deployments(claimRefNamespace).Get(ctx, controllerName, metav1.GetOptions{})

					if err != nil {
						log.Println("error: failed to get deployment " + controllerName + " in namespace " + claimRefNamespace + ": " + err.Error())
						continue
					}

					originalReplicas := deployment.Spec.Replicas

					deployment.Spec.Replicas = to.Int32Ptr(0)

					if _, err = client.AppsV1().Deployments(claimRefNamespace).Update(ctx, deployment, metav1.UpdateOptions{}); err != nil {
						log.Println("error: failed to scale down deployment " + controllerName + " in namespace " + claimRefNamespace + ": " + err.Error())
						continue
					}

					defer func() {
						deployment, err := client.AppsV1().Deployments(claimRefNamespace).Get(ctx, controllerName, metav1.GetOptions{})

						if err != nil {
							log.Println("error: failed to get object for deployment restore" + controllerName + " in namespace " + claimRefNamespace + ": " + err.Error())
						}

						deployment.Spec.Replicas = originalReplicas

						if _, err = client.AppsV1().Deployments(claimRefNamespace).Update(ctx, deployment, metav1.UpdateOptions{}); err != nil {
							log.Println("error: failed to restore deployment " + controllerName + " in namespace " + claimRefNamespace + " to " + strconv.Itoa(int(to.Int32(originalReplicas))) + " replicas: " + err.Error())
						}
					}()
				}

				log.Printf("successfully scaled %s %s/%s to %d replicas\n", controllerKind, claimRefNamespace, controllerName, 0)

				controllerRefs[controllerRefKey] = controllerRef{}
			}
		}

		log.Printf("generating new PV: %s/%s-csi.json", clustername, pvName)

		newPVName := fmt.Sprintf("%s-csi", pvName)

		newPV := &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{
				Name: newPVName,
				Annotations: map[string]string{
					"pv.kubernetes.io/provisioned-by": "disk.csi.azure.com",
				},
			},
			Spec: corev1.PersistentVolumeSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Capacity: corev1.ResourceList{
					corev1.ResourceStorage: capacityStorage,
				},
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{
						Driver:       "disk.csi.azure.com",
						VolumeHandle: azureDiskDiskURI,
						VolumeAttributes: map[string]string{
							"csi.storage.k8s.io/pv/name":       newPVName,
							"csi.storage.k8s.io/pvc/name":      claimRefName,
							"csi.storage.k8s.io/pvc/namespace": claimRefNamespace,
							"requestedsizegib":                 capacityStorage.String(),
							"skuname":                          skuName,
						},
					},
				},
				ClaimRef: &corev1.ObjectReference{
					Kind:       "PersistentVolumeClaim",
					Name:       claimRefName,
					Namespace:  claimRefNamespace,
					APIVersion: "v1",
				},
				PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimPolicy(persistentVolumeReclaimPolicy),
				StorageClassName:              newStorageClass,
			},
		}

		pvBytes, err := json.Marshal(pvName)

		if err != nil {
			log.Println("error: failed to marshal pv manifest for pv " + pvName + ": " + err.Error())
			continue
		}

		log.Printf("saving new PV manifest: %s/%s-csi.json\n", clustername, pvName)

		if err := os.WriteFile(fmt.Sprintf("%s/%s.json", clustername, pvName), pvBytes, 0644); err != nil {
			log.Println("error: failed to write pv manifest for pv " + pvName + ": " + err.Error())
			continue
		}

		log.Printf("applying new PV manifest: %s/%s-csi.json\n", clustername, pvName)

		if _, err = client.CoreV1().PersistentVolumes().Create(context.TODO(), newPV, metav1.CreateOptions{}); err != nil {
			log.Println("error: failed to apply pv manifest for pv " + pvName + ": " + err.Error())
			continue
		}

		log.Printf("pv created succcessfully: %s/%s-csi.json\n", clustername, pvName)

		existingPVC, err := client.CoreV1().PersistentVolumeClaims(claimRefNamespace).Get(ctx, claimRefName, metav1.GetOptions{})

		if err != nil {
			log.Println("error: failed to fetch existing pvc " + claimRefName + " in namespace " + claimRefNamespace + ": " + err.Error())
			continue
		}

		existingPVCBytes, err := json.Marshal(existingPVC)

		if err != nil {
			log.Println("error: failed to marshal existing pvc " + claimRefName + " from namespace " + claimRefNamespace + ": " + err.Error())
			continue
		}

		log.Printf("saving existing PVC manifest: %s/original-pvc.%s.%s.json\n", clustername, claimRefName, claimRefNamespace)

		if err := os.WriteFile(fmt.Sprintf("%s/original-pvc.%s.%s.json", clustername, claimRefName, claimRefNamespace), existingPVCBytes, 0644); err != nil {
			log.Println("error: failed to write existing pvc " + claimRefName + " from namespace " + claimRefNamespace + ": " + err.Error())
			continue
		}

		delete(existingPVC.Annotations, "pv.kubernetes.io/bind-completed")

		newPVC := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:        claimRefName,
				Namespace:   claimRefNamespace,
				Labels:      existingPVC.Labels,
				Annotations: existingPVC.Annotations,
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: existingPVC.Spec.AccessModes,
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: capacityStorage,
					},
				},
				StorageClassName: &newStorageClass,
				VolumeName:       newPVName,
			},
		}

		newPVCBytes, err := json.Marshal(newPVC)

		if err != nil {
			log.Println("error: failed to marshal new pvc " + claimRefName + " in namespace " + claimRefNamespace + ": " + err.Error())
			continue
		}

		log.Printf("saving new PVC manifest: %s/new-pvc.%s.%s.json\n", clustername, claimRefName, claimRefNamespace)

		if err := os.WriteFile(fmt.Sprintf("%s/new-pvc.%s.%s.json", clustername, claimRefName, claimRefNamespace), newPVCBytes, 0644); err != nil {
			log.Println("error: failed to write new pvc " + claimRefName + " in namespace " + claimRefNamespace + ": " + err.Error())
			continue
		}

		propagationPolicy := metav1.DeletePropagationForeground

		err = client.CoreV1().PersistentVolumeClaims(claimRefNamespace).Delete(context.Background(), claimRefName, metav1.DeleteOptions{
			PropagationPolicy: &propagationPolicy,
		})

		if err != nil {
			log.Println("error: failed to delete old pvc " + claimRefName + " in namespace " + claimRefNamespace + ": " + err.Error())
			continue
		}

		for {
			_, err := client.CoreV1().PersistentVolumeClaims(claimRefNamespace).Get(context.Background(), claimRefName, metav1.GetOptions{})

			if err != nil {
				if errors.IsNotFound(err) {
					break
				} else {
					log.Println("error: failed to get old pvc " + claimRefName + " in namespace " + claimRefNamespace + ": " + err.Error())
					continue
				}
			}

			log.Println("waiting for old pvc " + claimRefName + " in namespace " + claimRefNamespace + " to be deleted")
			time.Sleep(1 * time.Second)
		}

		log.Printf("applying new PVC manifest: %s/new-pvc.%s.%s.json\n", clustername, claimRefName, claimRefNamespace)

		if _, err = client.CoreV1().PersistentVolumeClaims(claimRefNamespace).Create(context.Background(), newPVC, metav1.CreateOptions{}); err != nil {
			log.Println("error: failed to create new pvc " + claimRefName + " in namespace " + claimRefNamespace + ": " + err.Error())
			continue
		}

		log.Printf("PVC successfully recreated: %s/%s\n", claimRefNamespace, claimRefName)
	}
}
