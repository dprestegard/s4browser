//go:build !darwin && !windows

package fsx

// renameNoReplace renames tmp to final unless final exists.
func renameNoReplace(tmp, final string) error { return renameNoReplaceLink(tmp, final) }
