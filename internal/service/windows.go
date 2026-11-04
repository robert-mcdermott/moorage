//go:build windows

package service

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// ServiceName is what Windows calls the server's service.
const ServiceName = "Moorage"

// account is who the service runs as: an account of its own that Windows looks after, with no password and few
// rights, and not LocalSystem, as the apps it starts run as it too.
const account = `NT SERVICE\` + ServiceName

// Windows is the Windows service manager.
type Windows struct {
	binary string
}

func forWindows(bool) (Manager, error) { return &Windows{}, nil }

// File says what was installed, as there is no file for it.
func (w *Windows) File() string {
	if w.binary != "" {
		return "the " + ServiceName + " service, running " + w.binary
	}
	return "the " + ServiceName + " service"
}

// Definition says what the service runs.
func (w *Windows) Definition() ([]byte, error) {
	m, s, err := open()
	if err != nil {
		return nil, err
	}
	defer m.Disconnect()
	defer s.Close()
	cfg, err := s.Config()
	if err != nil {
		return nil, err
	}
	return []byte(cfg.BinaryPathName), nil
}

// Render says what would be installed.
func (w *Windows) Render(spec Spec) ([]byte, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf("%q server run --config %q\nas %s\n", spec.Exe, spec.Config, account)), nil
}

func open() (*mgr.Mgr, *mgr.Service, error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, nil, elevated(err)
	}
	s, err := m.OpenService(ServiceName)
	if err != nil {
		m.Disconnect()
		return nil, nil, err
	}
	return m, s, nil
}

// elevated says what it takes, when that is what is missing.
func elevated(err error) error {
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return errors.New("this takes an elevated terminal: run it as an administrator")
	}
	return err
}

// Install creates the service, gives its account what it needs, and starts it if asked.
func (w *Windows) Install(spec Spec, start bool) error {
	if why := notOfItsOwn(spec.DataDir); why != "" {
		return fmt.Errorf("the service cannot have %s as its data directory, as that closes it to everyone else: %s. Set data_dir to a folder of its own", spec.DataDir, why)
	}
	if err := spec.Validate(); err != nil {
		return err
	}
	if !spec.System {
		return errors.New("a service is for the whole computer here: run this in an elevated terminal, as an administrator")
	}
	exe, err := reachableBinary(spec.Exe)
	if err != nil {
		return err
	}
	w.binary = exe
	m, err := mgr.Connect()
	if err != nil {
		return elevated(err)
	}
	defer m.Disconnect()
	args := []string{"server", "run", "--config", spec.Config}
	s, err := m.OpenService(ServiceName)
	if err == nil {
		// Installing again takes up where this binary and configuration are now.
		defer s.Close()
		cfg, err := s.Config()
		if err != nil {
			return err
		}
		cfg.BinaryPathName = commandLine(exe, args)
		if err := s.UpdateConfig(cfg); err != nil {
			return err
		}
	} else {
		s, err = m.CreateService(ServiceName, exe, mgr.Config{
			DisplayName:      "Moorage",
			Description:      "Runs and serves the apps deployed to this computer.",
			StartType:        mgr.StartAutomatic,
			ServiceStartName: account,
		}, args...)
		if err != nil {
			return elevated(err)
		}
		defer s.Close()
	}
	// It starts again by itself if it fails, as a unit on Linux does.
	again := mgr.RecoveryAction{Type: mgr.ServiceRestart, Delay: 2 * time.Second}
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{again, again, again}, 60); err != nil {
		return err
	}
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return err
	}
	if err := setPath(spec.Path); err != nil {
		return err
	}
	if err := keepPrivileges(s); err != nil {
		return err
	}
	if err := os.MkdirAll(spec.DataDir, 0o755); err != nil {
		return err
	}
	if err := grantToService(spec.DataDir); err != nil {
		return fmt.Errorf("give the service %s: %w", spec.DataDir, err)
	}
	// Its configuration may be in a folder with other things in it, which are no business of the service's, and whose
	// owner should not find themselves shut out of it. That folder is only closed if it is the one meant for this.
	config := spec.Config
	if dir := filepath.Dir(spec.Config); strings.EqualFold(dir, filepath.Join(os.Getenv("ProgramData"), ServiceName)) && notOfItsOwn(dir) == "" {
		config = dir
	}
	if err := grantToService(config); err != nil {
		return fmt.Errorf("give the service %s: %w", config, err)
	}
	if !start {
		return nil
	}
	return w.Start()
}

// commandLine quotes a command as Windows expects.
func commandLine(exe string, args []string) string {
	parts := []string{syscall.EscapeArg(exe)}
	for _, a := range args {
		parts = append(parts, syscall.EscapeArg(a))
	}
	return strings.Join(parts, " ")
}

// reachableBinary finds a place for the program that the service's account can read and no one else can change. What is
// in someone's own profile, where the installer puts it for them, is theirs alone, so that a copy goes where programs go.
func reachableBinary(exe string) (string, error) {
	programs := os.Getenv("ProgramFiles")
	if programs == "" {
		return "", errors.New("%ProgramFiles% is not set")
	}
	// Anywhere else, someone other than an administrator may be able to replace what the service runs.
	if strings.HasPrefix(strings.ToLower(exe), strings.ToLower(programs)+string(filepath.Separator)) {
		return exe, nil
	}
	dir := filepath.Join(programs, ServiceName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	target := filepath.Join(dir, "moor.exe")
	tmp, err := os.CreateTemp(dir, ".moor-*.exe")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	in, err := os.Open(exe)
	if err != nil {
		tmp.Close()
		return "", err
	}
	defer in.Close()
	_, err = io.Copy(tmp, in)
	if closed := tmp.Close(); err == nil {
		err = closed
	}
	if err != nil {
		return "", err
	}
	// One that is running can be moved aside, and cannot be written over.
	if _, err := os.Stat(target); err == nil {
		if err := os.Rename(target, fmt.Sprintf("%s.%d.old", target, time.Now().UnixNano())); err != nil {
			return "", err
		}
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return "", err
	}
	return target, nil
}

// setPath gives the service the path it runs with, and the apps after it.
func setPath(path string) error {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\`+ServiceName, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	return key.SetStringsValue("Environment", []string{"PATH=" + path})
}

// notOfItsOwn says why closing a folder to everyone but the service would take something away from someone, if it would.
func notOfItsOwn(dir string) string {
	clean, err := filepath.Abs(dir)
	if err != nil {
		return "it cannot be found"
	}
	clean = strings.ToLower(filepath.Clean(clean))
	volume := strings.ToLower(filepath.VolumeName(clean))
	if clean == volume || clean == volume+string(filepath.Separator) {
		return "it is the top of a drive"
	}
	for _, name := range []string{"USERPROFILE", "PUBLIC", "ALLUSERSPROFILE", "ProgramData", "ProgramFiles", "ProgramFiles(x86)", "SystemRoot", "SystemDrive", "APPDATA", "LOCALAPPDATA", "TEMP"} {
		place := os.Getenv(name)
		if place == "" {
			continue
		}
		place = strings.ToLower(filepath.Clean(place))
		if clean == place || strings.HasPrefix(place, clean+string(filepath.Separator)) {
			return "it has " + os.Getenv(name) + " in it, or is it"
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		if users := strings.ToLower(filepath.Dir(filepath.Clean(home))); clean == users {
			return "it is where people's own folders are"
		}
	}
	return ""
}

// keepPrivileges has the service keep only the privilege that it needs, as an account of this kind is otherwise given
// others, including impersonating whoever connects to it.
func keepPrivileges(s *mgr.Service) error {
	names, err := windows.UTF16FromString("SeChangeNotifyPrivilege")
	if err != nil {
		return err
	}
	names = append(names, 0) // a list of strings ends with an empty one
	info := struct{ names *uint16 }{&names[0]}
	return windows.ChangeServiceConfig2(s.Handle, windows.SERVICE_CONFIG_REQUIRED_PRIVILEGES_INFO, (*byte)(unsafe.Pointer(&info)))
}

// grantToService lets the service's account, the system and administrators in, and nobody else, with administrators as the
// owner. On a computer, what is under %ProgramData% can otherwise be read by everyone, and that would include the master
// key, and whoever made a folder there first would go on being able to let themselves in. It goes through everything that
// is in a folder, as what was closed to someone else by MakePrivate does not inherit from it.
func grantToService(path string) error {
	entries := []windows.EXPLICIT_ACCESS{}
	add := func(sid *windows.SID, kind windows.TRUSTEE_TYPE) {
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.SET_ACCESS,
			Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee:           windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: kind, TrusteeValue: windows.TrusteeValueFromSID(sid)},
		})
	}
	var owner *windows.SID
	for _, known := range []windows.WELL_KNOWN_SID_TYPE{windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid} {
		sid, err := windows.CreateWellKnownSid(known)
		if err != nil {
			return err
		}
		add(sid, windows.TRUSTEE_IS_WELL_KNOWN_GROUP)
		if known == windows.WinBuiltinAdministratorsSid {
			owner = sid
		}
	}
	sid, _, _, err := windows.LookupSID("", account)
	if err != nil {
		return err
	}
	add(sid, windows.TRUSTEE_IS_USER)
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return err
	}
	const what = windows.OWNER_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	return filepath.WalkDir(path, func(at string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		// A link would have this reach what it leads to, wherever that is. Only what is here is the service's.
		if !d.IsDir() && !d.Type().IsRegular() {
			return nil
		}
		return windows.SetNamedSecurityInfo(at, windows.SE_FILE_OBJECT, what, owner, nil, acl, nil)
	})
}

// Uninstall stops the service and removes it. Nothing it was given goes with it, for the data is still there.
func (w *Windows) Uninstall() error {
	m, s, err := open()
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return nil
		}
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	_ = stopped(s)
	return elevated(s.Delete())
}

// Start starts the server.
func (w *Windows) Start() error {
	m, s, err := open()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	err = s.Start()
	if errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return nil
	}
	return elevated(err)
}

// Stop stops the server and waits for it. The apps go on.
func (w *Windows) Stop() error {
	m, s, err := open()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	return stopped(s)
}

func stopped(s *mgr.Service) error {
	st, err := s.Control(svc.Stop)
	if errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
		return nil
	}
	if err != nil {
		return elevated(err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for st.State != svc.Stopped {
		if time.Now().After(deadline) {
			return errors.New("the service did not stop within a minute")
		}
		time.Sleep(200 * time.Millisecond)
		if st, err = s.Query(); err != nil {
			return err
		}
	}
	return nil
}

// Restart stops the server and starts it again.
func (w *Windows) Restart() error {
	if err := w.Stop(); err != nil {
		return err
	}
	return w.Start()
}

// Status reads the service's state, which anyone may.
func (w *Windows) Status() (Status, error) {
	var st Status
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return st, err
	}
	defer windows.CloseServiceHandle(scm)
	name, err := windows.UTF16PtrFromString(ServiceName)
	if err != nil {
		return st, err
	}
	h, err := windows.OpenService(scm, name, windows.SERVICE_QUERY_STATUS)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	defer windows.CloseServiceHandle(h)
	st.Installed = true
	var info windows.SERVICE_STATUS_PROCESS
	var needed uint32
	if err := windows.QueryServiceStatusEx(h, windows.SC_STATUS_PROCESS_INFO, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), &needed); err != nil {
		return st, err
	}
	st.PID = int(info.ProcessId)
	switch info.CurrentState {
	case windows.SERVICE_RUNNING:
		st.Running, st.Detail = true, "running"
	case windows.SERVICE_START_PENDING:
		st.Starting, st.Detail = true, "starting"
	case windows.SERVICE_STOP_PENDING:
		st.Detail = "stopping"
	default:
		st.Detail = "stopped"
	}
	return st, nil
}

// Logs says how to read the server's log.
func (w *Windows) Logs() string {
	return `the server's log in its data directory: logs\server\moorage.log`
}
