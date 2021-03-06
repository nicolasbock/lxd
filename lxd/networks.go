package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/gorilla/mux"
	log "github.com/lxc/lxd/shared/log15"
	"github.com/pkg/errors"

	lxd "github.com/lxc/lxd/client"
	"github.com/lxc/lxd/lxd/cluster"
	"github.com/lxc/lxd/lxd/db"
	"github.com/lxc/lxd/lxd/device/nictype"
	"github.com/lxc/lxd/lxd/instance"
	"github.com/lxc/lxd/lxd/network"
	"github.com/lxc/lxd/lxd/network/openvswitch"
	"github.com/lxc/lxd/lxd/project"
	"github.com/lxc/lxd/lxd/response"
	"github.com/lxc/lxd/lxd/revert"
	"github.com/lxc/lxd/lxd/state"
	"github.com/lxc/lxd/lxd/util"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/logger"
	"github.com/lxc/lxd/shared/version"
)

// Lock to prevent concurent networks creation
var networkCreateLock sync.Mutex

var networksCmd = APIEndpoint{
	Path: "networks",

	Get:  APIEndpointAction{Handler: networksGet, AccessHandler: allowAuthenticated},
	Post: APIEndpointAction{Handler: networksPost},
}

var networkCmd = APIEndpoint{
	Path: "networks/{name}",

	Delete: APIEndpointAction{Handler: networkDelete},
	Get:    APIEndpointAction{Handler: networkGet, AccessHandler: allowAuthenticated},
	Patch:  APIEndpointAction{Handler: networkPatch},
	Post:   APIEndpointAction{Handler: networkPost},
	Put:    APIEndpointAction{Handler: networkPut},
}

var networkLeasesCmd = APIEndpoint{
	Path: "networks/{name}/leases",

	Get: APIEndpointAction{Handler: networkLeasesGet, AccessHandler: allowAuthenticated},
}

var networkStateCmd = APIEndpoint{
	Path: "networks/{name}/state",

	Get: APIEndpointAction{Handler: networkStateGet, AccessHandler: allowAuthenticated},
}

// API endpoints
func networksGet(d *Daemon, r *http.Request) response.Response {
	recursion := util.IsRecursionRequest(r)

	ifs, err := networkGetInterfaces(d.cluster)
	if err != nil {
		return response.InternalError(err)
	}

	resultString := []string{}
	resultMap := []api.Network{}
	for _, iface := range ifs {
		if !recursion {
			resultString = append(resultString, fmt.Sprintf("/%s/networks/%s", version.APIVersion, iface))
		} else {
			net, err := doNetworkGet(d, iface)
			if err != nil {
				continue
			}
			resultMap = append(resultMap, net)
		}
	}

	if !recursion {
		return response.SyncResponse(true, resultString)
	}

	return response.SyncResponse(true, resultMap)
}

func networksPost(d *Daemon, r *http.Request) response.Response {
	networkCreateLock.Lock()
	defer networkCreateLock.Unlock()

	req := api.NetworksPost{}

	// Parse the request.
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	// Sanity checks.
	if req.Name == "" {
		return response.BadRequest(fmt.Errorf("No name provided"))
	}

	if req.Type == "" {
		req.Type = "bridge"
	}

	if req.Config == nil {
		req.Config = map[string]string{}
	}

	err = network.ValidateName(req.Name, req.Type)
	if err != nil {
		return response.BadRequest(err)
	}

	// Convert requested network type to DB type code.
	var dbNetType db.NetworkType
	switch req.Type {
	case "bridge":
		dbNetType = db.NetworkTypeBridge
	case "macvlan":
		dbNetType = db.NetworkTypeMacvlan
	case "sriov":
		dbNetType = db.NetworkTypeSriov
	default:
		return response.BadRequest(fmt.Errorf("Unrecognised network type"))
	}

	url := fmt.Sprintf("/%s/networks/%s", version.APIVersion, req.Name)
	resp := response.SyncResponseLocation(true, nil, url)

	if isClusterNotification(r) {
		// This is an internal request which triggers the actual creation of the network across all nodes
		// after they have been previously defined.
		err = doNetworksCreate(d, req, true)
		if err != nil {
			return response.SmartError(err)
		}
		return resp
	}

	targetNode := queryParam(r, "target")
	if targetNode != "" {
		// A targetNode was specified, let's just define the node's network without actually creating it.
		// Check that only NodeSpecificNetworkConfig keys are specified.
		for key := range req.Config {
			if !shared.StringInSlice(key, db.NodeSpecificNetworkConfig) {
				return response.BadRequest(fmt.Errorf("Config key %q may not be used as node-specific key", key))
			}
		}

		err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
			return tx.CreatePendingNetwork(targetNode, req.Name, dbNetType, req.Config)
		})
		if err != nil {
			if err == db.ErrAlreadyDefined {
				return response.BadRequest(fmt.Errorf("The network is already defined on node %q", targetNode))
			}
			return response.SmartError(err)
		}
		return resp
	}

	// Check if we're clustered.
	count, err := cluster.Count(d.State())
	if err != nil {
		return response.SmartError(err)
	}

	if count > 1 {
		err = networksPostCluster(d, req)
		if err != nil {
			return response.SmartError(err)
		}

		return resp
	}

	// Non-clustered network creation.
	err = network.FillConfig(&req)
	if err != nil {
		return response.SmartError(err)
	}

	networks, err := networkGetInterfaces(d.cluster)
	if err != nil {
		return response.InternalError(err)
	}

	if shared.StringInSlice(req.Name, networks) {
		return response.BadRequest(fmt.Errorf("The network already exists"))
	}

	revert := revert.New()
	defer revert.Fail()

	// Create the database entry.
	_, err = d.cluster.CreateNetwork(req.Name, req.Description, dbNetType, req.Config)
	if err != nil {
		return response.SmartError(errors.Wrapf(err, "Error inserting %q into database", req.Name))
	}

	revert.Add(func() {
		d.cluster.DeleteNetwork(req.Name)
	})

	// Create network and pass false to clusterNotification so the database record is removed on error.
	err = doNetworksCreate(d, req, false)
	if err != nil {
		return response.SmartError(err)
	}

	revert.Success()
	return resp
}

func networksPostCluster(d *Daemon, req api.NetworksPost) error {
	// Check that no node-specific config key has been defined.
	for key := range req.Config {
		if shared.StringInSlice(key, db.NodeSpecificNetworkConfig) {
			return fmt.Errorf("Config key %q is node-specific", key)
		}
	}

	// Check that the requested network type matches the type created when adding the local node config.
	// If network doesn't exist yet, ignore not found error, as this will be checked by NetworkNodeConfigs().
	_, netInfo, err := d.cluster.GetNetworkInAnyState(req.Name)
	if err != nil && err != db.ErrNoSuchObject {
		return err
	}

	if err != db.ErrNoSuchObject && req.Type != netInfo.Type {
		return fmt.Errorf("Requested network type %q doesn't match type in existing database record %q", req.Type, netInfo.Type)
	}

	// Add default values.
	err = network.FillConfig(&req)
	if err != nil {
		return err
	}

	// Check that the network is properly defined, get the node-specific configs and merge with global config.
	var configs map[string]map[string]string
	var nodeName string
	var networkID int64
	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		// Fetch the network ID.
		networkID, err = tx.GetNetworkID(req.Name)
		if err != nil {
			return err
		}

		// Fetch the node-specific configs.
		configs, err = tx.NetworkNodeConfigs(networkID)
		if err != nil {
			return err
		}

		// Take note of the name of this node.
		nodeName, err = tx.GetLocalNodeName()
		if err != nil {
			return err
		}

		// Insert the global config keys.
		return tx.CreateNetworkConfig(networkID, 0, req.Config)
	})
	if err != nil {
		if err == db.ErrNoSuchObject {
			return fmt.Errorf("Network not pending on any node (use --target <node> first)")
		}

		return err
	}

	// Create the network on this node.
	nodeReq := req
	for key, value := range configs[nodeName] {
		nodeReq.Config[key] = value
	}

	// Notify all other nodes to create the network.
	notifier, err := cluster.NewNotifier(d.State(), d.endpoints.NetworkCert(), cluster.NotifyAll)
	if err != nil {
		return err
	}

	revert := revert.New()
	defer revert.Fail()

	revert.Add(func() {
		d.cluster.Transaction(func(tx *db.ClusterTx) error {
			return tx.NetworkErrored(req.Name)
		})
	})

	// We need to mark the network as created now, because the network.LoadByName call invoked by
	// doNetworksCreate would fail with not-found otherwise.
	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		return tx.NetworkCreated(req.Name)
	})
	if err != nil {
		return err
	}

	err = doNetworksCreate(d, nodeReq, false)
	if err != nil {
		return err
	}

	err = notifier(func(client lxd.InstanceServer) error {
		server, _, err := client.GetServer()
		if err != nil {
			return err
		}

		nodeReq := req
		for key, value := range configs[server.Environment.ServerName] {
			nodeReq.Config[key] = value
		}

		return client.CreateNetwork(nodeReq)
	})
	if err != nil {
		return err
	}

	revert.Success()
	return nil
}

// Create the network on the system. The clusterNotification flag is used to indicate whether creation request
// is coming from a cluster notification (and if so we should not delete the database record on error).
func doNetworksCreate(d *Daemon, req api.NetworksPost, clusterNotification bool) error {
	// Start the network.
	n, err := network.LoadByName(d.State(), req.Name)
	if err != nil {
		return err
	}

	// Validate so that when run on a cluster node the full config (including node specific config) is checked.
	err = n.Validate(n.Config())
	if err != nil {
		return err
	}

	// Run initial creation setup for the network driver.
	err = n.Create(clusterNotification)
	if err != nil {
		return err
	}

	err = n.Start()
	if err != nil {
		n.Delete(clusterNotification)
		return err
	}

	return nil
}

func networkGet(d *Daemon, r *http.Request) response.Response {
	// If a target was specified, forward the request to the relevant node.
	resp := forwardedResponseIfTargetIsRemote(d, r)
	if resp != nil {
		return resp
	}

	name := mux.Vars(r)["name"]

	n, err := doNetworkGet(d, name)
	if err != nil {
		return response.SmartError(err)
	}

	targetNode := queryParam(r, "target")
	clustered, err := cluster.Enabled(d.db)
	if err != nil {
		return response.SmartError(err)
	}

	// If no target node is specified and the daemon is clustered, we omit
	// the node-specific fields.
	if targetNode == "" && clustered {
		for _, key := range db.NodeSpecificNetworkConfig {
			delete(n.Config, key)
		}
	}

	etag := []interface{}{n.Name, n.Managed, n.Type, n.Description, n.Config}

	return response.SyncResponseETag(true, &n, etag)
}

func doNetworkGet(d *Daemon, name string) (api.Network, error) {
	// Ignore veth pairs (for performance reasons)
	if strings.HasPrefix(name, "veth") {
		return api.Network{}, os.ErrNotExist
	}

	// Get some information
	osInfo, _ := net.InterfaceByName(name)
	_, dbInfo, _ := d.cluster.GetNetworkInAnyState(name)

	// Sanity check
	if osInfo == nil && dbInfo == nil {
		return api.Network{}, os.ErrNotExist
	}

	// Prepare the response
	n := api.Network{}
	n.Name = name
	n.UsedBy = []string{}
	n.Config = map[string]string{}

	// Set the device type as needed
	if osInfo != nil && shared.IsLoopback(osInfo) {
		n.Type = "loopback"
	} else if dbInfo != nil {
		n.Managed = true
		n.Description = dbInfo.Description
		n.Config = dbInfo.Config
		n.Type = dbInfo.Type
	} else if shared.PathExists(fmt.Sprintf("/sys/class/net/%s/bridge", n.Name)) {
		n.Type = "bridge"
	} else if shared.PathExists(fmt.Sprintf("/proc/net/vlan/%s", n.Name)) {
		n.Type = "vlan"
	} else if shared.PathExists(fmt.Sprintf("/sys/class/net/%s/device", n.Name)) {
		n.Type = "physical"
	} else if shared.PathExists(fmt.Sprintf("/sys/class/net/%s/bonding", n.Name)) {
		n.Type = "bond"
	} else {
		ovs := openvswitch.NewOVS()
		if exists, _ := ovs.BridgeExists(n.Name); exists {
			n.Type = "bridge"
		} else {
			n.Type = "unknown"
		}
	}

	// Look for containers using the interface
	if n.Type != "loopback" {
		// Look at instances.
		insts, err := instance.LoadFromAllProjects(d.State())
		if err != nil {
			return api.Network{}, err
		}

		for _, inst := range insts {
			inUse, err := network.IsInUseByInstance(d.State(), inst, n.Name)
			if err != nil {
				return api.Network{}, err
			}

			if inUse {
				uri := fmt.Sprintf("/%s/instances/%s", version.APIVersion, inst.Name())
				if inst.Project() != project.Default {
					uri += fmt.Sprintf("?project=%s", inst.Project())
				}
				n.UsedBy = append(n.UsedBy, uri)
			}
		}

		// Look for profiles.
		var profiles []db.Profile
		err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
			profiles, err = tx.GetProfiles(db.ProfileFilter{})
			if err != nil {
				return err
			}

			return nil
		})
		if err != nil {
			return api.Network{}, err
		}

		for _, profile := range profiles {
			inUse, err := network.IsInUseByProfile(d.State(), *db.ProfileToAPI(&profile), n.Name)
			if err != nil {
				return api.Network{}, err
			}

			if inUse {
				uri := fmt.Sprintf("/%s/profiles/%s", version.APIVersion, profile.Name)
				if profile.Project != project.Default {
					uri += fmt.Sprintf("?project=%s", profile.Project)
				}
				n.UsedBy = append(n.UsedBy, uri)
			}
		}
	}

	if dbInfo != nil {
		n.Status = dbInfo.Status
		n.Locations = dbInfo.Locations
	}

	return n, nil
}

func networkDelete(d *Daemon, r *http.Request) response.Response {
	name := mux.Vars(r)["name"]
	state := d.State()

	// Check if the network is pending, if so we just need to delete it from the database.
	_, dbNetwork, err := d.cluster.GetNetworkInAnyState(name)
	if err != nil {
		return response.SmartError(err)
	}
	if dbNetwork.Status == api.NetworkStatusPending {
		err := d.cluster.DeleteNetwork(name)
		if err != nil {
			return response.SmartError(err)
		}
		return response.EmptySyncResponse
	}

	// Get the existing network.
	n, err := network.LoadByName(state, name)
	if err != nil {
		return response.NotFound(err)
	}

	clusterNotification := false
	if isClusterNotification(r) {
		clusterNotification = true // We just want to delete the network from the system.
	} else {
		// Sanity checks
		inUse, err := n.IsUsed()
		if err != nil {
			return response.SmartError(err)
		}

		if inUse {
			return response.BadRequest(fmt.Errorf("The network is currently in use"))
		}

		// Notify all other nodes. If any node is down, an error will be returned.
		notifier, err := cluster.NewNotifier(d.State(), d.endpoints.NetworkCert(), cluster.NotifyAll)
		if err != nil {
			return response.SmartError(err)
		}
		err = notifier(func(client lxd.InstanceServer) error {
			return client.DeleteNetwork(name)
		})
		if err != nil {
			return response.SmartError(err)
		}
	}

	// Delete the network.
	err = n.Delete(clusterNotification)
	if err != nil {
		return response.SmartError(err)
	}

	// Cleanup storage.
	if shared.PathExists(shared.VarPath("networks", n.Name())) {
		os.RemoveAll(shared.VarPath("networks", n.Name()))
	}

	return response.EmptySyncResponse
}

func networkPost(d *Daemon, r *http.Request) response.Response {
	// FIXME: renaming a network is currently not supported in clustering
	//        mode. The difficulty is that network.Start() depends on the
	//        network having already been renamed in the database, which is
	//        a chicken-and-egg problem for cluster notifications (the
	//        serving node should typically do the database job, so the
	//        network is not yet renamed inthe db when the notified node
	//        runs network.Start).
	clustered, err := cluster.Enabled(d.db)
	if err != nil {
		return response.SmartError(err)
	}
	if clustered {
		return response.BadRequest(fmt.Errorf("Renaming a network not supported in LXD clusters"))
	}

	name := mux.Vars(r)["name"]
	req := api.NetworkPost{}
	state := d.State()

	// Parse the request
	err = json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	// Get the existing network
	n, err := network.LoadByName(state, name)
	if err != nil {
		return response.NotFound(err)
	}

	// Sanity checks
	if req.Name == "" {
		return response.BadRequest(fmt.Errorf("No name provided"))
	}

	err = network.ValidateName(req.Name, n.Type())
	if err != nil {
		return response.BadRequest(err)
	}

	// Check that the name isn't already in use
	networks, err := networkGetInterfaces(d.cluster)
	if err != nil {
		return response.InternalError(err)
	}

	if shared.StringInSlice(req.Name, networks) {
		return response.Conflict(fmt.Errorf("Network %q already exists", req.Name))
	}

	// Rename it
	err = n.Rename(req.Name)
	if err != nil {
		return response.SmartError(err)
	}

	return response.SyncResponseLocation(true, nil, fmt.Sprintf("/%s/networks/%s", version.APIVersion, req.Name))
}

func networkPut(d *Daemon, r *http.Request) response.Response {
	// If a target was specified, forward the request to the relevant node.
	resp := forwardedResponseIfTargetIsRemote(d, r)
	if resp != nil {
		return resp
	}

	name := mux.Vars(r)["name"]

	// Get the existing network.
	_, dbInfo, err := d.cluster.GetNetworkInAnyState(name)
	if err != nil {
		return response.SmartError(err)
	}

	targetNode := queryParam(r, "target")
	clustered, err := cluster.Enabled(d.db)
	if err != nil {
		return response.SmartError(err)
	}

	// If no target node is specified and the daemon is clustered, we omit the node-specific fields so that
	// the e-tag can be generated correctly. This is because the GET request used to populate the request
	// will also remove node-specific keys when no target is specified.
	if targetNode == "" && clustered {
		for _, key := range db.NodeSpecificNetworkConfig {
			delete(dbInfo.Config, key)
		}
	}

	// Validate the ETag.
	etag := []interface{}{dbInfo.Name, dbInfo.Managed, dbInfo.Type, dbInfo.Description, dbInfo.Config}
	err = util.EtagCheck(r, etag)
	if err != nil {
		return response.PreconditionFailed(err)
	}

	// Decode the request.
	req := api.NetworkPut{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return response.BadRequest(err)
	}

	// In clustered mode, we differentiate between node specific and non-node specific config keys based on
	// whether the user has specified a target to apply the config to.
	if clustered {
		if targetNode == "" {
			// If no target is specified, then ensure only non-node-specific config keys are changed.
			for k := range req.Config {
				if shared.StringInSlice(k, db.NodeSpecificNetworkConfig) {
					return response.BadRequest(fmt.Errorf("Config key %q is node-specific", k))
				}
			}
		} else {
			// If a target is specified, then ensure only node-specific config keys are changed.
			for k, v := range req.Config {
				if !shared.StringInSlice(k, db.NodeSpecificNetworkConfig) && dbInfo.Config[k] != v {
					return response.BadRequest(fmt.Errorf("Config key %q may not be used as node-specific key", k))
				}
			}
		}
	}

	return doNetworkUpdate(d, name, req, targetNode, isClusterNotification(r), r.Method, clustered)
}

func networkPatch(d *Daemon, r *http.Request) response.Response {
	return networkPut(d, r)
}

// doNetworkUpdate loads the current local network config, merges with the requested network config, validates
// and applies the changes. Will also notify other cluster nodes of non-node specific config if needed.
func doNetworkUpdate(d *Daemon, name string, req api.NetworkPut, targetNode string, clusterNotification bool, httpMethod string, clustered bool) response.Response {
	// Load the local node-specific network.
	n, err := network.LoadByName(d.State(), name)
	if err != nil {
		return response.NotFound(err)
	}

	if req.Config == nil {
		req.Config = map[string]string{}
	}

	// Normally a "put" request will replace all existing config, however when clustered, we need to account
	// for the node specific config keys and not replace them when the request doesn't specify a specific node.
	if targetNode == "" && httpMethod != http.MethodPatch && clustered {
		// If non-node specific config being updated via "put" method in cluster, then merge the current
		// node-specific network config with the submitted config to allow validation.
		// This allows removal of non-node specific keys when they are absent from request config.
		for k, v := range n.Config() {
			if shared.StringInSlice(k, db.NodeSpecificNetworkConfig) {
				req.Config[k] = v
			}
		}
	} else if httpMethod == http.MethodPatch {
		// If config being updated via "patch" method, then merge all existing config with the keys that
		// are present in the request config.
		for k, v := range n.Config() {
			_, ok := req.Config[k]
			if !ok {
				req.Config[k] = v
			}
		}
	}

	// Validate the merged configuration.
	err = network.Validate(name, n.Type(), req.Config)
	if err != nil {
		return response.BadRequest(err)
	}

	// Apply the new configuration (will also notify other cluster nodes if needed).
	err = n.Update(req, targetNode, clusterNotification)
	if err != nil {
		return response.SmartError(err)
	}

	return response.EmptySyncResponse
}

func networkLeasesGet(d *Daemon, r *http.Request) response.Response {
	name := mux.Vars(r)["name"]
	project := projectParam(r)

	// Try to get the network
	n, err := doNetworkGet(d, name)
	if err != nil {
		return response.SmartError(err)
	}

	// Validate that we do have leases for it
	if !n.Managed || n.Type != "bridge" {
		return response.NotFound(errors.New("Leases not found"))
	}

	leases := []api.NetworkLease{}
	projectMacs := []string{}

	// Get all static leases
	if !isClusterNotification(r) {
		// Get all the instances
		instances, err := instance.LoadByProject(d.State(), project)
		if err != nil {
			return response.SmartError(err)
		}

		for _, inst := range instances {
			// Go through all its devices (including profiles).
			for k, dev := range inst.ExpandedDevices() {
				// Skip uninteresting entries.
				if dev["type"] != "nic" {
					continue
				}

				nicType, err := nictype.NICType(d.State(), dev)
				if err != nil || nicType != "bridged" {
					continue
				}

				// Temporarily populate parent from network setting if used.
				if dev["network"] != "" {
					dev["parent"] = dev["network"]
				}

				if dev["parent"] != name {
					continue
				}

				// Fill in the hwaddr from volatile.
				if dev["hwaddr"] == "" {
					dev["hwaddr"] = inst.LocalConfig()[fmt.Sprintf("volatile.%s.hwaddr", k)]
				}

				// Record the MAC.
				if dev["hwaddr"] != "" {
					projectMacs = append(projectMacs, dev["hwaddr"])
				}

				// Add the lease.
				if dev["ipv4.address"] != "" {
					leases = append(leases, api.NetworkLease{
						Hostname: inst.Name(),
						Address:  dev["ipv4.address"],
						Hwaddr:   dev["hwaddr"],
						Type:     "static",
						Location: inst.Location(),
					})
				}

				if dev["ipv6.address"] != "" {
					leases = append(leases, api.NetworkLease{
						Hostname: inst.Name(),
						Address:  dev["ipv6.address"],
						Hwaddr:   dev["hwaddr"],
						Type:     "static",
						Location: inst.Location(),
					})
				}
			}
		}
	}

	// Local server name.
	var serverName string
	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		serverName, err = tx.GetLocalNodeName()
		return err
	})
	if err != nil {
		return response.SmartError(err)
	}

	// Get dynamic leases.
	leaseFile := shared.VarPath("networks", name, "dnsmasq.leases")
	if !shared.PathExists(leaseFile) {
		return response.SyncResponse(true, leases)
	}

	content, err := ioutil.ReadFile(leaseFile)
	if err != nil {
		return response.SmartError(err)
	}

	for _, lease := range strings.Split(string(content), "\n") {
		fields := strings.Fields(lease)
		if len(fields) >= 5 {
			// Parse the MAC.
			mac := network.GetMACSlice(fields[1])
			macStr := strings.Join(mac, ":")

			if len(macStr) < 17 && fields[4] != "" {
				macStr = fields[4][len(fields[4])-17:]
			}

			// Look for an existing static entry.
			found := false
			for _, entry := range leases {
				if entry.Hwaddr == macStr && entry.Address == fields[2] {
					found = true
					break
				}
			}

			if found {
				continue
			}

			// Add the lease to the list.
			leases = append(leases, api.NetworkLease{
				Hostname: fields[3],
				Address:  fields[2],
				Hwaddr:   macStr,
				Type:     "dynamic",
				Location: serverName,
			})
		}
	}

	// Collect leases from other servers.
	if !isClusterNotification(r) {
		notifier, err := cluster.NewNotifier(d.State(), d.endpoints.NetworkCert(), cluster.NotifyAlive)
		if err != nil {
			return response.SmartError(err)
		}

		err = notifier(func(client lxd.InstanceServer) error {
			memberLeases, err := client.GetNetworkLeases(name)
			if err != nil {
				return err
			}

			leases = append(leases, memberLeases...)
			return nil
		})
		if err != nil {
			return response.SmartError(err)
		}

		// Filter based on project.
		filteredLeases := []api.NetworkLease{}
		for _, lease := range leases {
			if !shared.StringInSlice(lease.Hwaddr, projectMacs) {
				continue
			}

			filteredLeases = append(filteredLeases, lease)
		}

		leases = filteredLeases
	}

	return response.SyncResponse(true, leases)
}

func networkStartup(s *state.State) error {
	// Get a list of managed networks.
	networks, err := s.Cluster.GetNonPendingNetworks()
	if err != nil {
		return err
	}

	// Bring them all up.
	for _, name := range networks {
		n, err := network.LoadByName(s, name)
		if err != nil {
			return err
		}

		err = n.Validate(n.Config())
		if err != nil {
			// Don't cause LXD to fail to start entirely on network start up failure.
			logger.Error("Failed to validate network", log.Ctx{"err": err, "name": name})
			continue
		}

		err = n.Start()
		if err != nil {
			// Don't cause LXD to fail to start entirely on network start up failure.
			logger.Error("Failed to bring up network", log.Ctx{"err": err, "name": name})
			continue
		}
	}

	return nil
}

func networkShutdown(s *state.State) error {
	// Get a list of managed networks
	networks, err := s.Cluster.GetNetworks()
	if err != nil {
		return err
	}

	// Bring them all up
	for _, name := range networks {
		n, err := network.LoadByName(s, name)
		if err != nil {
			return err
		}

		err = n.Stop()
		if err != nil {
			logger.Error("Failed to bring down network", log.Ctx{"err": err, "name": name})
		}
	}

	return nil
}

func networkStateGet(d *Daemon, r *http.Request) response.Response {
	// If a target was specified, forward the request to the relevant node.
	resp := forwardedResponseIfTargetIsRemote(d, r)
	if resp != nil {
		return resp
	}

	name := mux.Vars(r)["name"]

	// Get some information
	osInfo, _ := net.InterfaceByName(name)

	// Sanity check
	if osInfo == nil {
		return response.NotFound(fmt.Errorf("Interface '%s' not found", name))
	}

	return response.SyncResponse(true, networkGetState(*osInfo))
}
