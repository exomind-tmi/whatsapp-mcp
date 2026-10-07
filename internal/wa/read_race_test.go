//go:build race

package wa_test

// raceBuild is whether the race detector is on. It makes everything tens of times
// slower, so the big performance archive is not built then.
const raceBuild = true
