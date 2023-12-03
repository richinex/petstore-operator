/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"

	"github.com/pkg/errors"
	"google.golang.org/genproto/googleapis/type/date"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	petstorev1 "github.com/richinex/petstore-operator/api/v1alpha1"
	psclient "github.com/richinex/petstore-operator/client"
	pb "github.com/richinex/petstore-operator/client/proto"
)

const (
	PetFinalizer = "pet.petstore.example.com"
)

// PetReconciler reconciles a Pet object
type PetReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=petstore.example.com,resources=pets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=petstore.example.com,resources=pets/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=petstore.example.com,resources=pets/finalizers,verbs=update

// Reconcile moves the current state of the pet to be the desired state described in the pet.spec.
// Reconcile is a method on PetReconciler that implements the reconcile loop for the Pet custom resource.
func (r *PetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, errResult error) {
    // logger extracts the preconfigured logger from the context, useful for logging throughout this method.
    logger := log.FromContext(ctx)

    // pet is a new instance of the Pet custom resource.
    pet := &petstorev1.Pet{}

    // r.Get fetches the Pet resource from the API server based on the NamespacedName in the reconcile Request.
    // NamespacedName includes the namespace and the name of the resource.
    if err := r.Get(ctx, req.NamespacedName, pet); err != nil {
        // If the Pet resource is not found, log this information and return without error.
        // Returning nil error here will not requeue the request.
        if apierrors.IsNotFound(err) {
            logger.Info("object was not found")
            return reconcile.Result{}, nil
        }

        // If there was a different error fetching the Pet resource, log the error and return with the error.
        // Returning with an error will requeue the request to be tried again.
        logger.Error(err, "failed to fetch pet from API server")
        return ctrl.Result{}, err
    }

    // helper is used to create a patch that can be used to update the Pet resource.
    // patch.NewHelper is a utility function to ease the patching process of Kubernetes objects.
    helper, err := patch.NewHelper(pet, r.Client)
    if err != nil {
        // If there's an error creating the patch helper, return with an error to requeue the request.
        return ctrl.Result{}, errors.Wrap(err, "failed to create patch helper")
    }

    // Defer a function to ensure we patch the Pet resource with any changes made during reconcile.
    // This is done after the main logic of the function, right before exiting.
    defer func() {
        if err := helper.Patch(ctx, pet); err != nil {
            // If there's an error patching the Pet resource, store it in errResult.
            errResult = err
        }
    }()

    // Check if the DeletionTimestamp is set to zero (i.e., the resource is not being deleted).
    if pet.DeletionTimestamp.IsZero() {
        // If the pet is not being deleted, reconcile its desired state.
        return r.ReconcileNormal(ctx, pet)
    }

    // If the pet is being deleted (i.e., DeletionTimestamp is not zero), handle its deletion.
    return r.ReconcileDelete(ctx, pet)
}


// ReconcileNormal will ensure the finalizer and save the desired state to the petstore.
// It handles the normal reconciliation of a Pet resource. It's called when the Pet resource
// is not being deleted. It synchronizes the state of the Pet resource in the Kubernetes cluster with an
// external pet store system.
func (r *PetReconciler) ReconcileNormal(ctx context.Context, pet *petstorev1.Pet) (ctrl.Result, error) {
    // A logger is initialized with specific values (pet's name and ID) for easier tracing in logs.
    logger := ctrl.LoggerFrom(ctx).WithValues("pet", pet.Spec.Name, "id", pet.Status.ID)

    // Adding a finalizer to the Pet resource. Finalizers are used to perform cleanup operations
    // before Kubernetes garbage collects the resource. This is important for external dependencies or states.
    logger.Info("ensuring finalizer")
    controllerutil.AddFinalizer(pet, PetFinalizer)

    // Retrieve a client to interact with the external pet store system.
    psc, err := getPetstoreClient()
    if err != nil {
        return ctrl.Result{}, errors.Wrap(err, "unable to construct petstore client")
    }

    // Check if the pet already exists in the external pet store.
    logger.Info("finding pets in store")
    psPet, err := findPetInStore(ctx, psc, pet)
    if err != nil {
        return ctrl.Result{}, errors.Wrap(err, "failed trying to find pet in pet store")
    }

    // If the pet is not found in the external pet store (psPet is nil), create it there.
    if psPet == nil {
        logger.Info("pet was not found in store")
        err := createPetInStore(ctx, pet, psc)
        return ctrl.Result{}, err
    }

    // If the pet is found in the external pet store, update its information there.
    // This syncs the pet's state in Kubernetes with the external system.
    logger.Info("updating pet in store")
    if err := updatePetInStore(ctx, psc, pet, psPet.Pet); err != nil {
        return ctrl.Result{}, err
    }

    // Return with no error, indicating a successful reconciliation.
    return ctrl.Result{}, nil
}


// ReconcileDelete deletes the pet from the petstore and removes the finalizer.
// It handles the deletion of a Pet resource. It's called when the Pet resource
// has been marked for deletion in the Kubernetes cluster.
func (r *PetReconciler) ReconcileDelete(ctx context.Context, pet *petstorev1.Pet) (ctrl.Result, error) {
    // Retrieve a client to interact with the external pet store system.
    // This client is used to perform operations in the external pet store.
    psc, err := getPetstoreClient()
    if err != nil {
        // If unable to construct the petstore client, return an error.
        // This error will cause the reconcile to be requeued.
        return ctrl.Result{}, errors.Wrap(err, "unable to construct petstore client")
    }

    // Check if the Pet has an associated ID in the external pet store.
    if pet.Status.ID != "" {
        // If the ID is set, it means the pet exists in the external store, so delete it there.
        // This ensures the pet's state in Kubernetes is in sync with the external system.
        if err := psc.DeletePets(ctx, []string{pet.Status.ID}); err != nil {
            // If there's an error deleting the pet in the external store, return an error.
            // This error will cause the reconcile to be requeued.
            return ctrl.Result{}, errors.Wrap(err, "failed to delete pet")
        }
    }

    // Once the pet is successfully deleted from the external store, remove the finalizer.
    // Removing the finalizer allows Kubernetes to proceed with garbage collecting the resource.
    // This is an important step to ensure that the Pet resource is fully cleaned up in Kubernetes.
    controllerutil.RemoveFinalizer(pet, PetFinalizer)

    // Return with no error, indicating a successful delete reconciliation.
    return ctrl.Result{}, nil
}


// createPetInStore is a helper function that takes care of creating a new pet in an external pet store system.
// It takes the Kubernetes context, a reference to the Pet custom resource, and a client for the pet store.
func createPetInStore(ctx context.Context, pet *petstorev1.Pet, psc *psclient.Client) error {
    // pbPet is an instance of the Pet protobuf message (or similar structure) that will be sent to the pet store.
    // This is where the Pet custom resource data is translated into the format expected by the external pet store.
    pbPet := &pb.Pet{
        Name:     pet.Spec.Name,         // The name of the pet is taken from the Pet custom resource spec.
        Type:     petTypeToProtoPetType(pet.Spec.Type),  // Convert the pet type from CRD format to pet store's format.
        Birthday: timeToPbDate(pet.Spec.Birthday),       // Convert the birthday to the appropriate format.
    }

    // Add the new pet to the pet store using the AddPets method of the pet store client.
    // This operation communicates with the external pet store system to create the new pet.
    ids, err := psc.AddPets(ctx, []*pb.Pet{pbPet})
    if err != nil {
        // If there's an error during the creation, wrap the error with a message and return it.
        // This error will then be handled by the caller, likely leading to a requeue of the reconcile operation.
        return errors.Wrap(err, "failed to create new pet in store")
    }

    // Update the Pet custom resource's status with the ID returned from the pet store.
    // This ID is used to identify the pet in the external system and is crucial for future interactions.
    pet.Status.ID = ids[0]

    // Return nil to indicate the successful creation of the pet in the store.
    return nil
}


// updatePetInStore is a helper function for updating a pet's information in the external pet store system.
// It takes the Kubernetes context, a client for the pet store, the Pet custom resource, and a reference to
// the Pet data structure used by the pet store system.
func updatePetInStore(ctx context.Context, psc *psclient.Client, pet *petstorev1.Pet, pbPet *pb.Pet) error {
    // Update the fields of the pb.Pet structure (used by the pet store) based on the current state
    // of the pet as defined in the Pet custom resource. This involves converting the data to the
    // format expected by the pet store system.
    pbPet.Name = pet.Spec.Name             // Set the pet's name.
    pbPet.Type = petTypeToProtoPetType(pet.Spec.Type)  // Convert the pet type to the pet store's format.
    pbPet.Birthday = timeToPbDate(pet.Spec.Birthday)   // Convert the birthday to the appropriate format.

    // Send the updated pet information to the pet store system using the UpdatePets method of the pet store client.
    // This operation communicates with the external pet store system to update the pet's data.
    if err := psc.UpdatePets(ctx, []*pb.Pet{pbPet}); err != nil {
        // If there's an error during the update, wrap the error with a message and return it.
        // This error will then be handled by the caller, likely leading to a requeue of the reconcile operation.
        return errors.Wrap(err, "failed to update the pet in the store")
    }

    // Return nil to indicate the successful update of the pet in the store.
    return nil
}


// SetupWithManager sets up the controller with the Manager.
func (r *PetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&petstorev1.Pet{}).
		Complete(r)
}

// findPetInStore searches the pet store for a pet that matches the custom resource pet.
// findPetInStore searches for a pet in an external pet store system.
// It uses the pet's name and type from the Pet custom resource to perform the search.
func findPetInStore(ctx context.Context, psc *psclient.Client, pet *petstorev1.Pet) (*psclient.Pet, error) {
    // psc.SearchPets sends a search request to the pet store system.
    // The request includes the name and type of the pet to search for.
    petsChan, err := psc.SearchPets(ctx, &pb.SearchPetsReq{
        Names: []string{pet.Spec.Name},                           // Search by pet's name.
        Types: []pb.PetType{petTypeToProtoPetType(pet.Spec.Type)}, // Convert and search by pet's type.
    })

    // If there's an error during the search, return the error wrapped with a message.
    if err != nil {
        return nil, errors.Wrap(err, "failed searching for pet")
    }

    // Iterate over the results received on the petsChan channel.
    for pbPet := range petsChan {
        // Check if there's an error for this specific search result.
        if pbPet.Error() != nil {
            // Log the error using the logger from the context.
            logger := ctrl.LoggerFrom(ctx)
            logger.Error(err, "search chan error")
            continue  // Skip to the next result in case of error.
        }

        // If the ID of the pet in the search result matches the ID in the Pet custom resource's status,
        // it means this is the pet we are looking for.
        if pbPet.Id == pet.Status.ID {
            // Return a pointer to the found pet.
            return &pbPet, nil
        }
    }

    // If no matching pet is found, return nil. This is not necessarily an error,
    // it may simply indicate that no pet matches the search criteria in the store.
    return nil, nil
}


func petTypeToProtoPetType(petType petstorev1.PetType) pb.PetType {
	switch petType {
	case petstorev1.DogPetType:
		return pb.PetType_PTCanine
	case petstorev1.CatPetType:
		return pb.PetType_PTFeline
	case petstorev1.BirdPetType:
		return pb.PetType_PTBird
	default:
		return pb.PetType_PTReptile
	}
}

// timeToPbDate converts a metav1.Time (a common time format used in Kubernetes) to a *date.Date,
// which is likely a time format used by the external pet store system or a gRPC/protobuf service.
func timeToPbDate(t metav1.Time) *date.Date {
    // Create and return a new date.Date object (from protobuf or a similar package)
    // The Year, Month, and Day fields of the metav1.Time object are converted to int32
    // and used to construct the date.Date object.
    return &date.Date{
        Year:  int32(t.Year()),  // Convert year to int32
        Month: int32(t.Month()), // Convert month to int32 (note: Month returns a time.Month which is an int)
        Day:   int32(t.Day()),   // Convert day to int32
    }
}


// getPetstoreClient creates and returns a new client to interact with the pet store service.
// This client is used for operations like adding, updating, or deleting pets in the pet store.
func getPetstoreClient() (*psclient.Client, error) {
    // The New function of the psclient package is called with the address of the pet store service.
    // "petstore-service.petstore.svc.cluster.local:6742" is likely the internal service DNS name and port
    // in a Kubernetes cluster, pointing to the pet store service.
    return psclient.New("petstore-service.petstore.svc.cluster.local:6742")
}
