// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cosmosdbs

import (
	"context"
	"fmt"

	"github.com/Azure/azure-service-operator/api/v1alpha1"
	"github.com/Azure/azure-service-operator/pkg/errhelp"
	"github.com/Azure/azure-service-operator/pkg/helpers"
	"github.com/Azure/azure-service-operator/pkg/resourcemanager"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// Ensure ensures that cosmosdb is provisioned as specified
func (m *AzureCosmosDBManager) Ensure(ctx context.Context, obj runtime.Object, opts ...resourcemanager.ConfigOption) (bool, error) {
	options := &resourcemanager.Options{}
	for _, opt := range opts {
		opt(options)
	}

	if options.SecretClient != nil {
		m.SecretClient = options.SecretClient
	}

	instance, err := m.convert(obj)
	if err != nil {
		return false, err
	}

	hash := helpers.Hash256(instance.Spec)

	if instance.Status.SpecHash == hash && instance.Status.Provisioned {
		instance.Status.RequestedAt = nil
		return true, nil
	}
	instance.Status.Provisioned = false

	// get the instance and update status
	db, err := m.GetCosmosDB(ctx, instance.Spec.ResourceGroup, instance.Name)
	if err != nil {
		azerr := errhelp.NewAzureErrorAzureError(err)

		switch azerr.Type {
		case errhelp.ResourceGroupNotFoundErrorCode, errhelp.ParentNotFoundErrorCode:
			instance.Status.Provisioning = false
			instance.Status.Message = azerr.Error()
			instance.Status.State = "Waiting"
			return false, nil
		case errhelp.ResourceNotFound:
			//NO-OP, try to create
		default:
			instance.Status.Message = fmt.Sprintf("Unhandled error after Get %v", azerr.Error())
		}

	} else {
		instance.Status.ResourceId = *db.ID
		instance.Status.State = *db.ProvisioningState
	}

	if instance.Status.State == "Creating" {
		// avoid multiple CreateOrUpdate requests while resource is already creating
		return false, nil
	}

	if instance.Status.State == "Succeeded" && instance.Status.SpecHash == hash {
		// provisioning is complete, update the secrets
		if err = m.createOrUpdateAccountKeysSecret(ctx, instance); err != nil {
			instance.Status.Message = err.Error()
			return false, err
		}

		instance.Status.Message = resourcemanager.SuccessMsg
		instance.Status.Provisioning = false
		instance.Status.Provisioned = true
		return true, nil
	}

	if instance.Status.State == "Failed" {
		instance.Status.Message = "Failed to provision CosmosDB"
		instance.Status.Provisioning = false
		instance.Status.Provisioned = false
		return true, nil
	}

	tags := helpers.LabelsToTags(instance.GetLabels())
	accountName := instance.ObjectMeta.Name
	groupName := instance.Spec.ResourceGroup
	location := instance.Spec.Location
	kind := instance.Spec.Kind
	dbType := instance.Spec.Properties.DatabaseAccountOfferType

	db, err = m.CreateOrUpdateCosmosDB(ctx, groupName, accountName, location, kind, dbType, tags)
	if err != nil {
		azerr := errhelp.NewAzureErrorAzureError(err)

		switch azerr.Type {
		case errhelp.AsyncOpIncompleteError:
			instance.Status.State = "Creating"
			instance.Status.Message = "Resource request successfully submitted to Azure"
			return false, nil
		case errhelp.InvalidResourceLocation, errhelp.LocationNotAvailableForResourceType:
			instance.Status.Provisioning = false
			instance.Status.Message = azerr.Error()
			return true, nil
		case errhelp.ResourceGroupNotFoundErrorCode, errhelp.ParentNotFoundErrorCode:
			instance.Status.Provisioning = false
			instance.Status.Message = azerr.Error()
			return false, nil
		case errhelp.NotFoundErrorCode:
			if nameExists, err := m.CheckNameExistsCosmosDB(ctx, accountName); err != nil {
				instance.Status.Message = err.Error()
				return false, err
			} else if nameExists {
				instance.Status.Provisioning = false
				instance.Status.Message = "CosmosDB Account name already exists"
				return true, nil
			}
		default:
			instance.Status.Message = azerr.Error()
			return false, nil
		}
	}

	instance.Status.SpecHash = hash
	instance.Status.ResourceId = *db.ID
	instance.Status.State = *db.ProvisioningState
	instance.Status.Provisioned = true
	instance.Status.Provisioning = false
	instance.Status.Message = resourcemanager.SuccessMsg
	return false, nil
}

// Delete drops cosmosdb
func (m *AzureCosmosDBManager) Delete(ctx context.Context, obj runtime.Object, opts ...resourcemanager.ConfigOption) (bool, error) {
	options := &resourcemanager.Options{}
	for _, opt := range opts {
		opt(options)
	}

	if options.SecretClient != nil {
		m.SecretClient = options.SecretClient
	}

	instance, err := m.convert(obj)
	if err != nil {
		return false, err
	}

	// if the resource is in a failed state it was never created or could never be verified
	// so we skip attempting to delete the resrouce from Azure
	if instance.Status.FailedProvisioning {
		return false, nil
	}

	groupName := instance.Spec.ResourceGroup
	accountName := instance.ObjectMeta.Name

	// try to delete the cosmosdb instance & secrets
	_, err = m.DeleteCosmosDB(ctx, groupName, accountName)
	if err != nil {
		azerr := errhelp.NewAzureErrorAzureError(err)

		// this is likely to happen on first try due to not waiting for the future to complete
		if azerr.Type == errhelp.AsyncOpIncompleteError {
			instance.Status.Message = "Deletion request submitted successfully"
			return true, nil
		}

		notFoundErrors := []string{
			errhelp.NotFoundErrorCode,              // happens on first request after deletion succeeds
			errhelp.ResourceNotFound,               // happens on subsequent requests after deletion succeeds
			errhelp.ResourceGroupNotFoundErrorCode, // database doesn't exist in this resource group but the name exists globally
		}
		if helpers.ContainsString(notFoundErrors, azerr.Type) {
			return false, m.deleteAccountKeysSecret(ctx, instance)
		}

		// unhandled error
		instance.Status.Message = azerr.Error()
		return false, err
	}

	return false, m.deleteAccountKeysSecret(ctx, instance)
}

// GetParents returns the parents of cosmosdb
func (m *AzureCosmosDBManager) GetParents(obj runtime.Object) ([]resourcemanager.KubeParent, error) {
	instance, err := m.convert(obj)
	if err != nil {
		return nil, err
	}

	return []resourcemanager.KubeParent{
		{
			Key: types.NamespacedName{
				Namespace: instance.Namespace,
				Name:      instance.Spec.ResourceGroup,
			},
			Target: &v1alpha1.ResourceGroup{},
		},
	}, nil
}

// GetStatus gets the ASOStatus
func (m *AzureCosmosDBManager) GetStatus(obj runtime.Object) (*v1alpha1.ASOStatus, error) {
	instance, err := m.convert(obj)
	if err != nil {
		return nil, err
	}
	return &instance.Status, nil
}

func (m *AzureCosmosDBManager) convert(obj runtime.Object) (*v1alpha1.CosmosDB, error) {
	db, ok := obj.(*v1alpha1.CosmosDB)
	if !ok {
		return nil, fmt.Errorf("failed type assertion on kind: %s", obj.GetObjectKind().GroupVersionKind().String())
	}
	return db, nil
}

func (m *AzureCosmosDBManager) createOrUpdateAccountKeysSecret(ctx context.Context, instance *v1alpha1.CosmosDB) error {
	result, err := m.ListKeys(ctx, instance.Spec.ResourceGroup, instance.ObjectMeta.Name)
	if err != nil {
		return err
	}

	secretKey := types.NamespacedName{
		Name:      instance.Name,
		Namespace: instance.Namespace,
	}
	secretData := map[string][]byte{
		"primaryConnectionString":    []byte(*result.PrimaryMasterKey),
		"secondaryConnectionString":  []byte(*result.SecondaryMasterKey),
		"primaryReadonlyMasterKey":   []byte(*result.PrimaryReadonlyMasterKey),
		"secondaryReadonlyMasterKey": []byte(*result.SecondaryReadonlyMasterKey),
	}

	return m.SecretClient.Upsert(ctx, secretKey, secretData)
}

func (m *AzureCosmosDBManager) deleteAccountKeysSecret(ctx context.Context, instance *v1alpha1.CosmosDB) error {
	secretKey := types.NamespacedName{
		Name:      instance.Name,
		Namespace: instance.Namespace,
	}
	return m.SecretClient.Delete(ctx, secretKey)
}
