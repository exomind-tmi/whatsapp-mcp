//go:build !race

package archive

// raceBuild is whether the race detector is on. It makes everything tens of times
// slower, so the big performance fixture is not built then (see perfFixture.get).
const raceBuild = false
