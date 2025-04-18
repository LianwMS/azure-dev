// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package azapi

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/cognitiveservices/armcognitiveservices"
)

// GetCognitiveAccount finds the cognitive account within a subscription
func (cli *AzureClient) GetCognitiveAccount(
	ctx context.Context,
	subscriptionId string,
	resourceGroupName string,
	accountName string) (armcognitiveservices.Account, error) {
	client, err := cli.createCognitiveAccountClient(ctx, subscriptionId)
	if err != nil {
		return armcognitiveservices.Account{}, err
	}

	response, err := client.Get(ctx, resourceGroupName, accountName, nil)
	if err != nil {
		return armcognitiveservices.Account{}, err
	}

	return response.Account, nil
}

// PurgeCognitiveAccount starts purge operation and wait until it is completed.
func (cli *AzureClient) PurgeCognitiveAccount(
	ctx context.Context, subscriptionId, location, resourceGroup, accountName string) error {
	client, err := cli.createDeletedCognitiveAccountClient(ctx, subscriptionId)
	if err != nil {
		return err
	}

	poller, err := client.BeginPurge(ctx, location, resourceGroup, accountName, nil)
	if err != nil {
		return fmt.Errorf("starting purging cognitive account: %w", err)
	}

	_, err = poller.PollUntilDone(ctx, nil)
	if err != nil {
		return fmt.Errorf("purging cognitive account: %w", err)
	}

	return nil
}

func (cli *AzureClient) createCognitiveAccountClient(
	ctx context.Context, subscriptionId string) (*armcognitiveservices.AccountsClient, error) {
	credential, err := cli.credentialProvider.CredentialForSubscription(ctx, subscriptionId)
	if err != nil {
		return nil, err
	}

	client, err := armcognitiveservices.NewAccountsClient(subscriptionId, credential, cli.armClientOptions)
	if err != nil {
		return nil, fmt.Errorf("creating Resource client: %w", err)
	}

	return client, nil
}

func (cli *AzureClient) createDeletedCognitiveAccountClient(
	ctx context.Context, subscriptionId string) (*armcognitiveservices.DeletedAccountsClient, error) {
	credential, err := cli.credentialProvider.CredentialForSubscription(ctx, subscriptionId)
	if err != nil {
		return nil, err
	}

	client, err := armcognitiveservices.NewDeletedAccountsClient(subscriptionId, credential, cli.armClientOptions)
	if err != nil {
		return nil, fmt.Errorf("creating Resource client: %w", err)
	}

	return client, nil
}

func (cli *AzureClient) GetAiModels(
	ctx context.Context,
	subscriptionId string,
	location string) ([]*armcognitiveservices.Model, error) {
	client, err := cli.createModelsClient(ctx, subscriptionId)
	if err != nil {
		return nil, err
	}

	modelsPager := client.NewListPager(location, nil)
	var models []*armcognitiveservices.Model
	for modelsPager.More() {
		page, err := modelsPager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		models = append(models, page.Value...)
	}

	return models, nil
}

func (cli *AzureClient) createModelsClient(
	ctx context.Context, subscriptionId string) (*armcognitiveservices.ModelsClient, error) {
	credential, err := cli.credentialProvider.CredentialForSubscription(ctx, subscriptionId)
	if err != nil {
		return nil, err
	}

	client, err := armcognitiveservices.NewModelsClient(subscriptionId, credential, cli.armClientOptions)
	if err != nil {
		return nil, fmt.Errorf("creating Resource client: %w", err)
	}

	return client, nil
}

func (cli *AzureClient) GetAiUsages(
	ctx context.Context,
	subscriptionId string,
	location string) ([]*armcognitiveservices.Usage, error) {
	client, err := cli.createUsagesClient(ctx, subscriptionId)
	if err != nil {
		return nil, err
	}

	modelsPager := client.NewListPager(location, nil)
	var models []*armcognitiveservices.Usage
	for modelsPager.More() {
		page, err := modelsPager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		models = append(models, page.Value...)
	}

	return models, nil
}

func (cli *AzureClient) createUsagesClient(
	ctx context.Context, subscriptionId string) (*armcognitiveservices.UsagesClient, error) {
	credential, err := cli.credentialProvider.CredentialForSubscription(ctx, subscriptionId)
	if err != nil {
		return nil, err
	}

	client, err := armcognitiveservices.NewUsagesClient(subscriptionId, credential, cli.armClientOptions)
	if err != nil {
		return nil, fmt.Errorf("creating Resource client: %w", err)
	}

	return client, nil
}
