package deploy

import (
	"debug/buildinfo"
	"debug/elf"
	"fmt"
	"runtime/debug"
)

// BuildID is what makes two portitor binaries one release: the module version
// and VCS revision Go stamps into every build. It is read from the file, never
// by running it — a linux binary cannot run on a darwin host.
type BuildID struct {
	Version  string
	Revision string
	Modified bool
}

func buildID(bi *debug.BuildInfo) BuildID {
	id := BuildID{Version: bi.Main.Version}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			id.Revision = s.Value
		case "vcs.modified":
			id.Modified = s.Value == "true"
		}
	}
	return id
}

// OwnBuildID is the running binary's BuildID.
func OwnBuildID() (BuildID, error) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return BuildID{}, fmt.Errorf("the running binary carries no build info")
	}
	return buildID(bi), nil
}

// FileBuildID reads a Go binary's BuildID from its file.
func FileBuildID(path string) (BuildID, error) {
	bi, err := buildinfo.ReadFile(path)
	if err != nil {
		return BuildID{}, fmt.Errorf("read build info of %s: %w", path, err)
	}
	return buildID(bi), nil
}

// elfMachine maps a Go arch to the ELF machine its linux build carries.
var elfMachine = map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}

// checkGateBinary refuses a gate binary that is not a linux executable for arch
// or is not the same build as own.
func checkGateBinary(path, arch string, own BuildID) error {
	want, ok := elfMachine[arch]
	if !ok {
		return fmt.Errorf("unsupported architecture %q (portitor ships amd64 and arm64)", arch)
	}
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("gate binary %s is not a linux executable: %w", path, err)
	}
	defer f.Close()
	if f.Machine != want || f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN {
		return fmt.Errorf("gate binary %s is %s %s, want a linux %s executable", path, f.Machine, f.Type, arch)
	}
	if f.OSABI != elf.ELFOSABI_NONE && f.OSABI != elf.ELFOSABI_LINUX {
		return fmt.Errorf("gate binary %s targets %s, want linux", path, f.OSABI)
	}
	got, err := FileBuildID(path)
	if err != nil {
		return err
	}
	if got.Version != own.Version || got.Revision != own.Revision {
		return fmt.Errorf("gate binary %s is build %s (%s), this portitor is %s (%s): reinstall or upgrade the pair",
			path, got.Version, got.Revision, own.Version, own.Revision)
	}
	return nil
}
