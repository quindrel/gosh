package server

type (
	// GetRequest represents request params for get server endpoint.
	GetRequest struct {
		ServerName string `json:"name"`
	}

	// DeleteRequest represents a request to delete a Server.
	DeleteRequest struct {
		Name string `json:"name"`
	}

	// UpgradeRequest represents a request to upgrade a Server.
	UpgradeRequest struct {
		Name string `json:"name"`
		Plan string `json:"plan"`
	}

	// UpdateRequest represents a request to update a Server.
	UpdateRequest struct {
		Name  string `json:"name"`
		Label string `json:"label"`
	}

	// CommitDiskChangesRequest represents request params for CommitDiskChanges server endpoint.
	CommitDiskChangesRequest struct {
		ServerName string `json:"name"`
	}

	// CreateRequest represents a request to create a Server.
	CreateRequest struct {
		ClientID    string        `json:"client_id"`
		Label       string        `json:"label"`
		Location    string        `json:"location"`
		ProductCode string        `json:"product_code"`
		Image       string        `json:"image"`
		Params      ParamsOptions `json:"params"`
	}

	// ParamsOptions represents the additional parameters in the request to create a Server.
	ParamsOptions struct {
		Name      string   `json:"name,omitempty"`
		IPv4      []string `json:"ipv4"`
		IPv6      []string `json:"ipv6,omitempty"`
		SSHKeys   []string `json:"ssh_keys,omitempty"`
		ContactID string   `json:"contact_id,omitempty"`
		Backup    string   `json:"backup,omitempty"`
		SendEmail string   `json:"send_email,omitempty"`
	}

	// GetStateOptions represents request params for the get_state
	// endpoint.
	GetStateOptions struct {
		Name string `url:"name"`
	}

	// ListUpgradesOptions represents request params for the
	// list_upgrades endpoint.
	ListUpgradesOptions struct {
		Name string `url:"name"`
	}

	// GenerateNetworkConfigOptions represents request params for the
	// generate_network_config endpoint.
	GenerateNetworkConfigOptions struct {
		Name string `url:"name"`
	}

	// AddIPOptions describes an IP address to add to a server.
	// The API uses "param" (not "address") for the IP value.
	AddIPOptions struct {
		Name string `url:"name"`
		IP   string `url:"param"`
	}

	// RemoveIPOptions describes an IP address to remove from a
	// server. The API uses "address" here (distinct from add_ip's
	// "param") — the inconsistency is the API's, not gosh's.
	RemoveIPOptions struct {
		Name string `url:"name"`
		IP   string `url:"address"`
	}

	// SetPrimaryIPOptions describes the new primary IP for a
	// server. Uses "address" like remove_ip.
	SetPrimaryIPOptions struct {
		Name string `url:"name"`
		IP   string `url:"address"`
	}

	// ChangeStateOptions describes a server state transition.
	// Valid State values: "power_on", "power_off", "rescue_on",
	// "rescue_off", "reboot".
	ChangeStateOptions struct {
		Name  string `url:"name"`
		State string `url:"state"`
	}

	// CanProvisionOptions checks resource availability for
	// provisioning a server. Product, Location, and Distro are
	// required; Arch is optional.
	CanProvisionOptions struct {
		Product  string `url:"product"`
		Location string `url:"location"`
		Distro   string `url:"distro"`
		Arch     string `url:"arch,omitempty"`
	}
)
