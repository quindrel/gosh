// Package db provides access to the `/cloud/db` API endpoint.
//
// SiteHost runs MySQL/MariaDB as a shared service per Cloud Container
// Server. You don't provision the DB engine — SiteHost does. This
// package creates logical databases on those shared engines and
// associates them with the www stacks that consume them.
//
// Naming model:
//
//   - MySQLHost is the public image code of the shared engine
//     (e.g. "mariadb1108", "mysql84"). It's pulled from the same
//     catalog that backs cloud/stack/image/list_all. Not a stack
//     name and not a hostname.
//   - Container is the **www** stack Name that owns/uses the database.
//     Recorded so the SiteHost UI groups them together; also so the
//     www container's `nz.sitehost.container.dbs=["<host>.<db>"]`
//     compose label resolves to a working hostname inside the
//     stack's network.
//   - Database is the logical database name created on the shared
//     engine, scoped per client_id.
//
// Companion packages:
//
//   - cloud/db/user — create/delete users on the shared engine, with
//     password and grants per database.
//   - cloud/db/grant — adjust grants on existing user/database pairs.
package db
