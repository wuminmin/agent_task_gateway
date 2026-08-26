//go:build !taskgate_scale

package gateway

// scaleLaneBuild is false in the acceptance run.
//
// The registry helpers use it to decide whether to materialise the publications
// whose committed input carries no rows. Those are scanned out of the Business
// database and compiled: five of the Catalog's seven take that path, measured at
// 25.84 GB peak on a 30 GB host. Only the cases that prepare an ordinal-program
// plan need them, and those live behind taskgate_scale.
const scaleLaneBuild = false
