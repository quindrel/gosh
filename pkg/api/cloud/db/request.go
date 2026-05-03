package db

import "github.com/sitehostnz/gosh/pkg/models"

type (
	// AddRequest adds/creates a new logical database on a shared
	// SiteHost MySQL/MariaDB engine running on the target CCS.
	AddRequest struct {
		// ServerName is the CCS name (cloud.server, e.g. "ch-faraday").
		ServerName string `url:"server_name"`
		// MySQLHost is the public image code of the shared DB engine
		// SiteHost runs on the CCS — e.g. "mariadb1108", "mysql84".
		// Pulled from cloud/stack/image/list_all. Not a stack name.
		MySQLHost string `url:"mysql_host"`
		// Database is the logical database name to create.
		Database string `url:"database"`
		// Container is the **www** stack Name that owns the database
		// (the consumer, not the engine). Recorded so the SiteHost
		// UI links the two and the www container's
		// `nz.sitehost.container.dbs=["<host>.<db>"]` label resolves.
		Container string `url:"container"`
	}
	// DeleteRequest a request to delete the database.
	DeleteRequest struct {
		ServerName string `url:"server_name"`
		MySQLHost  string `url:"mysql_host"`
		Database   string `url:"database"`
	}
	// UpdateRequest changes the **www** stack associated with an
	// existing database (i.e. retargets which stack owns it for the
	// purposes of UI grouping and the dbs label).
	UpdateRequest struct {
		ServerName string `url:"server_name"`
		MySQLHost  string `url:"mysql_host"`
		Database   string `url:"database"`
		// Container is the new owning www stack Name. Same semantics
		// as AddRequest.Container.
		Container string `url:"params[container]'"`
	}
	// ListOptions are options for filtering/listing databases.
	ListOptions struct {
		ServerName string `url:"filters[server_name],omitempty"`
		MySQLHost  string `url:"filters[mysql_host],omitempty"`
		Database   string `url:"filters[db_name],omitempty"`

		models.Filtering
	}
	// GetRequest is for getting a single database.
	GetRequest struct {
		ServerName string `json:"server_name"`
		MySQLHost  string `json:"mysql_host"`
		Database   string `json:"database"`
	}
)
