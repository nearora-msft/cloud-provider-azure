/*
Copyright 2020 The Kubernetes Authors.

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

package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2020-12-01/compute"
	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2021-02-01/network"
	"github.com/Azure/azure-sdk-for-go/services/privatedns/mgmt/2018-09-01/privatedns"
	"github.com/Azure/azure-sdk-for-go/services/storage/mgmt/2021-02-01/storage"
	"github.com/Azure/go-autorest/autorest/to"

	"k8s.io/klog/v2"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

// SkipMatchingTag skip account matching tag
const SkipMatchingTag = "skip-matching"
const LocationGlobal = "global"
const GroupIDFile = "file"
const PrivateDNSZoneName = "privatelink.file.core.windows.net"

// AccountOptions contains the fields which are used to create storage account.
type AccountOptions struct {
	Name, Type, Kind, ResourceGroup, Location string
	// indicate whether create new account when Name is empty
	EnableHTTPSTrafficOnly                  bool
	CreateAccount                           bool
	EnableLargeFileShare                    bool
	CreatePrivateEndpoint                   bool
	DisableFileServiceDeleteRetentionPolicy bool
	IsHnsEnabled                            *bool
	EnableNfsV3                             *bool
	Tags                                    map[string]string
	VirtualNetworkResourceIDs               []string
}

type accountWithLocation struct {
	Name, StorageType, Location string
}

// getStorageAccounts get matching storage accounts
func (az *Cloud) getStorageAccounts(accountOptions *AccountOptions) ([]accountWithLocation, error) {
	if az.StorageAccountClient == nil {
		return nil, fmt.Errorf("StorageAccountClient is nil")
	}
	ctx, cancel := getContextWithCancel()
	defer cancel()
	result, rerr := az.StorageAccountClient.ListByResourceGroup(ctx, accountOptions.ResourceGroup)
	if rerr != nil {
		return nil, rerr.Error()
	}

	accounts := []accountWithLocation{}
	for _, acct := range result {
		if acct.Name != nil && acct.Location != nil && acct.Sku != nil {
			if !(isStorageTypeEqual(acct, accountOptions) &&
				isAccountKindEqual(acct, accountOptions) &&
				isLocationEqual(acct, accountOptions) &&
				AreVNetRulesEqual(acct, accountOptions) &&
				isLargeFileSharesPropertyEqual(acct, accountOptions) &&
				isTaggedWithSkip(acct) &&
				isHnsPropertyEqual(acct, accountOptions) &&
				isEnableNfsV3PropertyEqual(acct, accountOptions) &&
				isPrivateEndpointAsExpected(acct, accountOptions)) {
				continue
			}
			accounts = append(accounts, accountWithLocation{Name: *acct.Name, StorageType: string((*acct.Sku).Name), Location: *acct.Location})
		}
	}
	return accounts, nil
}

// GetStorageAccesskey gets the storage account access key
func (az *Cloud) GetStorageAccesskey(account, resourceGroup string) (string, error) {
	if az.StorageAccountClient == nil {
		return "", fmt.Errorf("StorageAccountClient is nil")
	}

	ctx, cancel := getContextWithCancel()
	defer cancel()
	result, rerr := az.StorageAccountClient.ListKeys(ctx, resourceGroup, account)
	if rerr != nil {
		return "", rerr.Error()
	}
	if result.Keys == nil {
		return "", fmt.Errorf("empty keys")
	}

	for _, k := range *result.Keys {
		if k.Value != nil && *k.Value != "" {
			v := *k.Value
			if ind := strings.LastIndex(v, " "); ind >= 0 {
				v = v[(ind + 1):]
			}
			return v, nil
		}
	}
	return "", fmt.Errorf("no valid keys")
}

// EnsureStorageAccount search storage account, create one storage account(with genAccountNamePrefix) if not found, return accountName, accountKey
func (az *Cloud) EnsureStorageAccount(accountOptions *AccountOptions, genAccountNamePrefix string) (string, string, error) {
	if accountOptions == nil {
		return "", "", fmt.Errorf("account options is nil")
	}
	accountName := accountOptions.Name
	accountType := accountOptions.Type
	accountKind := accountOptions.Kind
	resourceGroup := accountOptions.ResourceGroup
	location := accountOptions.Location
	enableHTTPSTrafficOnly := accountOptions.EnableHTTPSTrafficOnly

	if len(accountName) == 0 {
		if !accountOptions.CreateAccount {
			// find a storage account that matches accountType
			accounts, err := az.getStorageAccounts(accountOptions)
			if err != nil {
				return "", "", fmt.Errorf("could not list storage accounts for account type %s: %w", accountType, err)
			}

			if len(accounts) > 0 {
				accountName = accounts[0].Name
				klog.V(4).Infof("found a matching account %s type %s location %s", accounts[0].Name, accounts[0].StorageType, accounts[0].Location)
			}
		}

		if len(accountName) == 0 {
			// set network rules for storage account
			var networkRuleSet *storage.NetworkRuleSet
			virtualNetworkRules := []storage.VirtualNetworkRule{}
			for i, subnetID := range accountOptions.VirtualNetworkResourceIDs {
				vnetRule := storage.VirtualNetworkRule{
					VirtualNetworkResourceID: &accountOptions.VirtualNetworkResourceIDs[i],
					Action:                   storage.ActionAllow,
				}
				virtualNetworkRules = append(virtualNetworkRules, vnetRule)
				klog.V(4).Infof("subnetID(%s) has been set", subnetID)
			}
			if len(virtualNetworkRules) > 0 {
				networkRuleSet = &storage.NetworkRuleSet{
					VirtualNetworkRules: &virtualNetworkRules,
					DefaultAction:       storage.DefaultActionDeny,
				}
			}

			// not found a matching account, now create a new account in current resource group
			accountName = generateStorageAccountName(genAccountNamePrefix)
			if location == "" {
				location = az.Location
			}
			if accountType == "" {
				accountType = consts.DefaultStorageAccountType
			}

			// use StorageV2 by default per https://docs.microsoft.com/en-us/azure/storage/common/storage-account-options
			kind := consts.DefaultStorageAccountKind
			if accountKind != "" {
				kind = storage.Kind(accountKind)
			}
			if len(accountOptions.Tags) == 0 {
				accountOptions.Tags = make(map[string]string)
			}
			accountOptions.Tags["created-by"] = "azure"
			tags := convertMapToMapPointer(accountOptions.Tags)

			klog.V(2).Infof("azure - no matching account found, begin to create a new account %s in resource group %s, location: %s, accountType: %s, accountKind: %s, tags: %+v",
				accountName, resourceGroup, location, accountType, kind, accountOptions.Tags)

			cp := storage.AccountCreateParameters{
				Sku:  &storage.Sku{Name: storage.SkuName(accountType)},
				Kind: kind,
				AccountPropertiesCreateParameters: &storage.AccountPropertiesCreateParameters{
					EnableHTTPSTrafficOnly: &enableHTTPSTrafficOnly,
					NetworkRuleSet:         networkRuleSet,
					IsHnsEnabled:           accountOptions.IsHnsEnabled,
					EnableNfsV3:            accountOptions.EnableNfsV3,
				},
				Tags:     tags,
				Location: &location}

			if accountOptions.EnableLargeFileShare {
				klog.V(2).Infof("Enabling LargeFileShare for the storage account")
				cp.AccountPropertiesCreateParameters.LargeFileSharesState = storage.LargeFileSharesStateEnabled
			}
			if az.StorageAccountClient == nil {
				return "", "", fmt.Errorf("StorageAccountClient is nil")
			}
			ctx, cancel := getContextWithCancel()
			defer cancel()
			rerr := az.StorageAccountClient.Create(ctx, resourceGroup, accountName, cp)
			if rerr != nil {
				return "", "", fmt.Errorf("failed to create storage account %s, error: %v", accountName, rerr)
			}

			if accountOptions.DisableFileServiceDeleteRetentionPolicy {
				klog.V(2).Infof("disable DisableFileServiceDeleteRetentionPolicy on account(%s), resource group(%s)", accountName, resourceGroup)
				prop, err := az.FileClient.GetServiceProperties(resourceGroup, accountName)
				if err != nil {
					return "", "", err
				}
				if prop.FileServicePropertiesProperties == nil {
					return "", "", fmt.Errorf("FileServicePropertiesProperties of account(%s), resource group(%s) is nil", accountName, resourceGroup)
				}
				prop.FileServicePropertiesProperties.ShareDeleteRetentionPolicy = &storage.DeleteRetentionPolicy{Enabled: to.BoolPtr(false)}
				if _, err := az.FileClient.SetServiceProperties(resourceGroup, accountName, prop); err != nil {
					return "", "", err
				}
			}

			if accountOptions.CreatePrivateEndpoint {
				// Create private endpoint
				privateEndpointName := accountName + "-pvtendpoint"
				if err := az.createPrivateEndpoint(ctx, accountName, privateEndpointName); err != nil {
					return "", "", fmt.Errorf("Failed to create private endpoint for storage account(%s), resourceGroup(%s), error: %v", accountName, az.ResourceGroup, err)
				}

				// Create DNS zone
				if err := az.createPrivateDNSZone(ctx); err != nil {
					return "", "", fmt.Errorf("Failed to create private DNS zone(%s) in resourceGroup(%s), error: %v", PrivateDNSZoneName, az.ResourceGroup, err)
				}

				// Create virtual link to the zone private DNS zone
				vNetLinkName := accountName + "-vnetlink"
				if err := az.createVNetLink(ctx, vNetLinkName); err != nil {
					return "", "", fmt.Errorf("Failed to create virtual link for vnet(%s) and DNS Zone(%s) in resourceGroup(%s), error: %v", az.VnetName, PrivateDNSZoneName, az.ResourceGroup, err)
				}

				// Create dns zone group
				dnsZoneGroupName := accountName + "-dnszonegroup"
				if err := az.createPrivateDNSZoneGroup(ctx, dnsZoneGroupName, privateEndpointName); err != nil {
					return "", "", fmt.Errorf("Failed to create private DNS zone group - privateEndpoint(%s), vNetName(%s), resourceGroup(%s), error: %v", privateEndpointName, az.VnetName, az.ResourceGroup, err)
				}
			}
		}
	}

	// find the access key with this account
	accountKey, err := az.GetStorageAccesskey(accountName, resourceGroup)
	if err != nil {
		return "", "", fmt.Errorf("could not get storage key for storage account %s: %w", accountName, err)
	}

	return accountName, accountKey, nil
}

func (az *Cloud) createPrivateEndpoint(ctx context.Context, accountName string, privateEndpointName string) error {
	klog.V(5).Infof("Creating private endpoint(%s) for account (%s)", privateEndpointName, accountName)
	subnet, rerr := az.SubnetsClient.Get(ctx, az.resourceGroup, az.cloud.VnetName, az.cloud.SubnetName, "")
	if rerr != nil {
		return rerr.Error()
	}

	storageAccount, rerr := az.StorageAccountClient.GetProperties(ctx, az.ResourceGroup, accountName)
	if rerr != nil {
		return rerr.Error()
	}

	// Disable the private endpoint network policies before creating private endpoint
	subnet.SubnetPropertiesFormat.PrivateEndpointNetworkPolicies = network.VirtualNetworkPrivateEndpointNetworkPoliciesDisabled
	rerr = az.SubnetsClient.CreateOrUpdate(ctx, az.ResourceGroup, az.VnetName, az.SubnetName, subnet)
	if rerr != nil {
		return rerr.Error()
	}

	//Create private endpoint
	privateLinkServiceConnectionName := accountName + "-pvtsvcconn"
	privateLinkServiceConnection := network.PrivateLinkServiceConnection{
		Name: &privateLinkServiceConnectionName,
		PrivateLinkServiceConnectionProperties: &network.PrivateLinkServiceConnectionProperties{
			GroupIds:             &[]string{GroupIDFile},
			PrivateLinkServiceID: storageAccount.ID,
		},
	}
	privateLinkServiceConnections := []network.PrivateLinkServiceConnection{privateLinkServiceConnection}
	privateEndpoint := network.PrivateEndpoint{
		Location:                  &az.Location,
		PrivateEndpointProperties: &network.PrivateEndpointProperties{Subnet: &subnet, PrivateLinkServiceConnections: &privateLinkServiceConnections},
	}
	err := az.privateendpointclient.CreateOrUpdate(ctx, az.resourceGroup, privateEndpointName, privateEndpoint, true)
	if err != nil {
		return err
	}
	return nil
}

func (az *Cloud) createPrivateDNSZone(ctx context.Context) error {
	klog.V(5).Infof("Creating private dns zone(%s) in resourceGroup (%s)", PrivateDNSZoneName, az.ResourceGroup)
	location := LocationGlobal
	privateDNSZone := privatedns.PrivateZone{Location: &location}
	err := az.privatednsclient.CreateOrUpdate(ctx, az.ResourceGroup, PrivateDNSZoneName, privateDNSZone, true)
	if err != nil {
		return err
	}
	return nil
}

func (az *Cloud) createVNetLink(ctx context.Context, vNetLinkName string) error {
	klog.V(5).Infof("Creating virtual link for vnet(%s) and DNS Zone(%s) in resourceGroup(%s)", vNetLinkName, PrivateDNSZoneName, az.ResourceGroup)
	location := LocationGlobal
	vnetID := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/virtualNetworks/%s", az.subscriptionID, az.ResourceGroup, az.VnetName)
	registrationEnabled := false
	parameters := privatedns.VirtualNetworkLink{
		Location: &location,
		VirtualNetworkLinkProperties: &privatedns.VirtualNetworkLinkProperties{
			VirtualNetwork:      &privatedns.SubResource{ID: &vnetID},
			RegistrationEnabled: &registrationEnabled},
	}
	err := az.virtualNetworkLinksClient.CreateOrUpdate(ctx, az.ResourceGroup, PrivateDNSZoneName, vNetLinkName, parameters, false)
	if err != nil {
		return err
	}
	return nil
}

func (az *Cloud) createPrivateDNSZoneGroup(ctx context.Context, dnsZoneGroupName string, privateEndpointName string) error {
	klog.V(5).Infof("Creating private DNS zone group(%s) with privateEndpoint(%s), vNetName(%s), resourceGroup(%s)", dnsZoneGroupName, privateEndpointName, az.VnetName, az.ResourceGroup)
	privateDNSZoneID := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/privateDnsZones/%s", az.subscriptionID, az.ResourceGroup, PrivateDNSZoneName)
	dnsZoneName := PrivateDNSZoneName
	privateDNSZoneConfig := network.PrivateDNSZoneConfig{
		Name: &dnsZoneName,
		PrivateDNSZonePropertiesFormat: &network.PrivateDNSZonePropertiesFormat{
			PrivateDNSZoneID: &privateDNSZoneID},
	}
	privateDNSZoneConfigs := []network.PrivateDNSZoneConfig{privateDNSZoneConfig}
	privateDNSZoneGroup := network.PrivateDNSZoneGroup{
		PrivateDNSZoneGroupPropertiesFormat: &network.PrivateDNSZoneGroupPropertiesFormat{
			PrivateDNSZoneConfigs: &privateDNSZoneConfigs,
		},
	}
	err := az.privatednszonegroupclient.CreateOrUpdate(ctx, az.ResourceGroup, privateEndpointName, dnsZoneGroupName, privateDNSZoneGroup, false)
	if err != nil {
		return err
	}
	return nil
}

// AddStorageAccountTags add tags to storage account
func (az *Cloud) AddStorageAccountTags(resourceGroup, account string, tags map[string]*string) *retry.Error {
	if az.StorageAccountClient == nil {
		return retry.NewError(false, fmt.Errorf("StorageAccountClient is nil"))
	}
	ctx, cancel := getContextWithCancel()
	defer cancel()
	result, rerr := az.StorageAccountClient.GetProperties(ctx, resourceGroup, account)
	if rerr != nil {
		return rerr
	}

	newTags := result.Tags
	if newTags == nil {
		newTags = make(map[string]*string)
	}

	// merge two tag map
	for k, v := range tags {
		newTags[k] = v
	}

	updateParams := storage.AccountUpdateParameters{Tags: newTags}
	return az.StorageAccountClient.Update(ctx, resourceGroup, account, updateParams)
}

// RemoveStorageAccountTag remove tag from storage account
func (az *Cloud) RemoveStorageAccountTag(resourceGroup, account, key string) *retry.Error {
	if az.StorageAccountClient == nil {
		return retry.NewError(false, fmt.Errorf("StorageAccountClient is nil"))
	}
	ctx, cancel := getContextWithCancel()
	defer cancel()
	result, rerr := az.StorageAccountClient.GetProperties(ctx, resourceGroup, account)
	if rerr != nil {
		return rerr
	}

	if len(result.Tags) == 0 {
		return nil
	}

	originalLen := len(result.Tags)
	delete(result.Tags, key)
	if originalLen != len(result.Tags) {
		updateParams := storage.AccountUpdateParameters{Tags: result.Tags}
		return az.StorageAccountClient.Update(ctx, resourceGroup, account, updateParams)
	}
	return nil
}

func isStorageTypeEqual(account storage.Account, accountOptions *AccountOptions) bool {
	if accountOptions.Type != "" && !strings.EqualFold(accountOptions.Type, string((*account.Sku).Name)) {
		return false
	}
	return true
}

func isAccountKindEqual(account storage.Account, accountOptions *AccountOptions) bool {
	if accountOptions.Kind != "" && !strings.EqualFold(accountOptions.Kind, string(account.Kind)) {
		return false
	}
	return true
}

func isLocationEqual(account storage.Account, accountOptions *AccountOptions) bool {
	if accountOptions.Location != "" && !strings.EqualFold(accountOptions.Location, *account.Location) {
		return false
	}
	return true
}

func AreVNetRulesEqual(account storage.Account, accountOptions *AccountOptions) bool {
	if len(accountOptions.VirtualNetworkResourceIDs) > 0 {
		if account.AccountProperties == nil || account.AccountProperties.NetworkRuleSet == nil ||
			account.AccountProperties.NetworkRuleSet.VirtualNetworkRules == nil {
			return false
		}

		found := false
		for _, subnetID := range accountOptions.VirtualNetworkResourceIDs {
			for _, rule := range *account.AccountProperties.NetworkRuleSet.VirtualNetworkRules {
				if strings.EqualFold(to.String(rule.VirtualNetworkResourceID), subnetID) && rule.Action == storage.ActionAllow {
					found = true
					break
				}
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func isLargeFileSharesPropertyEqual(account storage.Account, accountOptions *AccountOptions) bool {
	if account.Sku.Tier != storage.SkuTier(compute.PremiumLRS) && accountOptions.EnableLargeFileShare && (len(account.LargeFileSharesState) == 0 || account.LargeFileSharesState == storage.LargeFileSharesStateDisabled) {
		return false
	}
	return true
}

func isTaggedWithSkip(account storage.Account) bool {
	if account.Tags != nil {
		// skip account with SkipMatchingTag tag
		if _, ok := account.Tags[SkipMatchingTag]; ok {
			klog.V(2).Infof("found %s tag for account %s, skip matching", SkipMatchingTag, *account.Name)
			return false
		}
	}
	return true
}

func isHnsPropertyEqual(account storage.Account, accountOptions *AccountOptions) bool {
	return to.Bool(account.IsHnsEnabled) == to.Bool(accountOptions.IsHnsEnabled)
}

func isEnableNfsV3PropertyEqual(account storage.Account, accountOptions *AccountOptions) bool {
	return to.Bool(account.EnableNfsV3) == to.Bool(accountOptions.EnableNfsV3)
}

func isPrivateEndpointAsExpected(account storage.Account, accountOptions *AccountOptions) bool {
	if accountOptions.CreatePrivateEndpoint && len(*account.PrivateEndpointConnections) == 0 {
		return false
	}
	return true
}
