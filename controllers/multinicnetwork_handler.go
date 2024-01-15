/*
 * Copyright 2022- IBM Inc. All rights reserved
 * SPDX-License-Identifier: Apache2.0
 */

package controllers

import (
	"context"
	"fmt"
	"sync"

	"sigs.k8s.io/controller-runtime/pkg/client"

	multinicv1 "github.com/foundation-model-stack/multi-nic-cni/api/v1"
	"github.com/foundation-model-stack/multi-nic-cni/controllers/vars"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

var (
	RouteMessage map[multinicv1.RouteStatus]string = map[multinicv1.RouteStatus]string{
		multinicv1.SomeRouteFailed: "some route cannot be applied, need attention",
		multinicv1.RouteUnknown:    "some daemon cannot be connected",
		multinicv1.ApplyingRoute:   "waiting for route update",
		multinicv1.AllRouteApplied: "",
	}
)

// MultiNicNetworkHandler handles MultiNicNetwork object
// - update MultiNicNetwork status according to CIDR results
type MultiNicNetworkHandler struct {
	client.Client
	syncFlag bool
	sync.Mutex
	*SafeCache
}

func (h *MultiNicNetworkHandler) GetNetwork(name string) (*multinicv1.MultiNicNetwork, error) {
	instance := &multinicv1.MultiNicNetwork{}
	namespacedName := types.NamespacedName{
		Name:      name,
		Namespace: metav1.NamespaceAll,
	}
	ctx, cancel := context.WithTimeout(context.Background(), vars.ContextTimeout)
	defer cancel()
	err := h.Client.Get(ctx, namespacedName, instance)
	return instance, err
}

func (h *MultiNicNetworkHandler) SyncAllStatus(name string, spec multinicv1.CIDRSpec, routeStatus multinicv1.RouteStatus, daemonSize, infoAvailableSize int, cidrChange bool) (multinicv1.MultiNicNetworkStatus, error) {
	instance, err := h.GetNetwork(name)
	if err != nil {
		return multinicv1.MultiNicNetworkStatus{}, err
	}
	if h.syncFlag {
		return instance.Status, fmt.Errorf("syncFlag is set (skip SyncAllStatus to avoid congestion)")
	}
	h.Mutex.Lock()
	h.syncFlag = true
	discoverStatus := instance.Status.DiscoverStatus
	netConfigStatus := instance.Status.NetConfigStatus
	message := instance.Status.Message
	if routeStatus == multinicv1.SomeRouteFailed || routeStatus == multinicv1.ApplyingRoute {
		netConfigStatus = multinicv1.WaitForConfig
	} else if routeStatus == multinicv1.AllRouteApplied {
		netConfigStatus = multinicv1.ConfigComplete
	}

	if routeErrMsg, found := RouteMessage[routeStatus]; found {
		message = routeErrMsg
	}

	discoverStatus = multinicv1.DiscoverStatus{
		ExistDaemon:            daemonSize,
		InterfaceInfoAvailable: infoAvailableSize,
		CIDRProcessedHost:      discoverStatus.CIDRProcessedHost,
	}

	updatedResult, err := h.updateStatus(instance, spec, routeStatus, discoverStatus, netConfigStatus, message, cidrChange)
	h.syncFlag = false
	h.Mutex.Unlock()
	return updatedResult, err
}

func (h *MultiNicNetworkHandler) updateStatus(instance *multinicv1.MultiNicNetwork, spec multinicv1.CIDRSpec, status multinicv1.RouteStatus, discoverStatus multinicv1.DiscoverStatus, netConfigStatus multinicv1.NetConfigStatus, message string, cidrChange bool) (multinicv1.MultiNicNetworkStatus, error) {
	results := []multinicv1.NicNetworkResult{}

	if cidrChange {
		maxNumOfHost := 0
		for _, entry := range spec.CIDRs {
			numOfHost := len(entry.Hosts)
			result := multinicv1.NicNetworkResult{
				NetAddress: entry.NetAddress,
				NumOfHost:  numOfHost,
			}
			if numOfHost > maxNumOfHost {
				maxNumOfHost = numOfHost
			}
			results = append(results, result)
		}
		discoverStatus.CIDRProcessedHost = maxNumOfHost
	} else {
		results = instance.Status.ComputeResults
	}

	netStatus := multinicv1.MultiNicNetworkStatus{
		ComputeResults:  results,
		LastSyncTime:    metav1.Now(),
		DiscoverStatus:  discoverStatus,
		NetConfigStatus: netConfigStatus,
		Message:         message,
		RouteStatus:     status,
	}

	if !NetStatusUpdated(instance, netStatus) {
		vars.NetworkLog.V(2).Info(fmt.Sprintf("No status update %s", instance.Name))
		return netStatus, nil
	}

	vars.NetworkLog.V(2).Info(fmt.Sprintf("Update %s status", instance.Name))
	instance.Status = netStatus
	ctx, cancel := context.WithTimeout(context.Background(), vars.ContextTimeout)
	defer cancel()
	err := h.Client.Status().Update(ctx, instance)
	if err != nil {
		vars.NetworkLog.V(2).Info(fmt.Sprintf("Failed to update %s status: %v", instance.Name, err))
	} else {
		h.SetStatusCache(instance.Name, instance.Status)
	}
	return netStatus, err
}

func NetStatusUpdated(instance *multinicv1.MultiNicNetwork, newStatus multinicv1.MultiNicNetworkStatus) bool {
	prevStatus := instance.Status
	if prevStatus.Message != newStatus.Message || prevStatus.RouteStatus != newStatus.RouteStatus || prevStatus.NetConfigStatus != newStatus.NetConfigStatus || prevStatus.DiscoverStatus != newStatus.DiscoverStatus {
		return true
	}
	if len(prevStatus.ComputeResults) != len(newStatus.ComputeResults) {
		return true
	}
	prevComputeMap := make(map[string]int)
	for _, status := range prevStatus.ComputeResults {
		prevComputeMap[status.NetAddress] = status.NumOfHost
	}
	for _, status := range newStatus.ComputeResults {
		if numOfHost, found := prevComputeMap[status.NetAddress]; !found {
			return true
		} else if numOfHost != status.NumOfHost {
			return true
		}
	}
	return false
}

func (h *MultiNicNetworkHandler) UpdateNetConfigStatus(instance *multinicv1.MultiNicNetwork, netConfigStatus multinicv1.NetConfigStatus, message string) error {
	if message != "" {
		instance.Status.Message = message
	}
	if instance.Status.ComputeResults == nil {
		instance.Status.ComputeResults = []multinicv1.NicNetworkResult{}
	}
	instance.Status.NetConfigStatus = netConfigStatus
	ctx, cancel := context.WithTimeout(context.Background(), vars.ContextTimeout)
	defer cancel()
	err := h.Client.Status().Update(ctx, instance)
	if err != nil {
		vars.NetworkLog.V(2).Info(fmt.Sprintf("Failed to update %s status: %v", instance.Name, err))
	} else {
		h.SetStatusCache(instance.Name, instance.Status)
	}
	return err
}

func (h *MultiNicNetworkHandler) SetStatusCache(key string, value multinicv1.MultiNicNetworkStatus) {
	h.SafeCache.SetCache(key, value)
}

func (h *MultiNicNetworkHandler) GetStatusCache(key string) (multinicv1.MultiNicNetworkStatus, error) {
	value := h.SafeCache.GetCache(key)
	if value == nil {
		return multinicv1.MultiNicNetworkStatus{}, fmt.Errorf(vars.NotFoundError)
	}
	return value.(multinicv1.MultiNicNetworkStatus), nil
}

func (h *MultiNicNetworkHandler) ListStatusCache() map[string]multinicv1.MultiNicNetworkStatus {
	snapshot := make(map[string]multinicv1.MultiNicNetworkStatus)
	h.SafeCache.Lock()
	for key, value := range h.cache {
		snapshot[key] = value.(multinicv1.MultiNicNetworkStatus)
	}
	h.SafeCache.Unlock()
	return snapshot
}
