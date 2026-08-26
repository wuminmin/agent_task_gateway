//go:build taskgate_scale

package gateway

// scaleLaneBuild is true on the scale lane, where the cases that resolve every
// snapshot publication the Catalog declares actually run.
const scaleLaneBuild = true
