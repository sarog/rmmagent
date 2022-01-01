package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	ps "github.com/elastic/go-sysinfo"
	"github.com/go-resty/resty/v2"
	"github.com/gonutz/w32/v2"
	nats "github.com/nats-io/nats.go"
	wapf "github.com/sarog/go-win64api"
	rmm "github.com/sarog/rmmagent/shared"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	getDriveType = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetDriveTypeW")
)

const BrandingFolder = "TacticalAgent"

// WindowsAgent struct
type WindowsAgent struct {
	Hostname      string
	Arch          string
	AgentID       string
	BaseURL       string
	ApiURL        string
	Token         string
	AgentPK       int
	Cert          string
	ProgramDir    string
	EXE           string
	SystemDrive   string
	Nssm          string
	MeshInstaller string
	MeshSystemEXE string
	MeshSVC       string
	PyBin         string
	Headers       map[string]string
	Logger        *logrus.Logger
	Version       string
	Debug         bool
	rClient       *resty.Client
}

// New Initializes a new WindowsAgent with logger
func New(logger *logrus.Logger, version string) *WindowsAgent {
	host, _ := ps.Host()
	info := host.Info()
	pd := filepath.Join(os.Getenv("ProgramFiles"), BrandingFolder)
	exe := filepath.Join(pd, "tacticalrmm.exe") // todo: 2021-12-31: branding
	dbFile := filepath.Join(pd, "agentdb.db")   // deprecated
	sd := os.Getenv("SystemDrive")
	nssm, mesh := ArchInfo(pd)

	var pybin string
	switch runtime.GOARCH {
	case "amd64":
		pybin = filepath.Join(pd, "py38-x64", "python.exe")
	case "386":
		pybin = filepath.Join(pd, "py38-x32", "python.exe")
	}

	// Previous Python agent database
	if FileExists(dbFile) {
		os.Remove(dbFile)
	}

	var (
		baseurl string
		agentid string
		apiurl  string
		token   string
		agentpk string
		pk      int
		cert    string
	)

	// todo: 2021-12-31: migrate to DPAPI?
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\TacticalRMM`, registry.ALL_ACCESS)
	if err == nil {
		baseurl, _, err = k.GetStringValue("BaseURL")
		if err != nil {
			logger.Fatalln("Unable to get BaseURL:", err)
		}

		agentid, _, err = k.GetStringValue("AgentID")
		if err != nil {
			logger.Fatalln("Unable to get AgentID:", err)
		}

		apiurl, _, err = k.GetStringValue("ApiURL")
		if err != nil {
			logger.Fatalln("Unable to get ApiURL:", err)
		}

		token, _, err = k.GetStringValue("Token")
		if err != nil {
			logger.Fatalln("Unable to get Token:", err)
		}

		agentpk, _, err = k.GetStringValue("AgentPK")
		if err != nil {
			logger.Fatalln("Unable to get AgentPK:", err)
		}

		pk, _ = strconv.Atoi(agentpk)

		cert, _, _ = k.GetStringValue("Cert")
	}

	headers := make(map[string]string)
	if len(token) > 0 {
		headers["Content-Type"] = "application/json"
		headers["Authorization"] = fmt.Sprintf("Token %s", token)
	}

	restyC := resty.New()
	restyC.SetBaseURL(baseurl)
	restyC.SetCloseConnection(true)
	restyC.SetHeaders(headers)
	restyC.SetTimeout(15 * time.Second)
	restyC.SetDebug(logger.IsLevelEnabled(logrus.DebugLevel))
	if len(cert) > 0 {
		restyC.SetRootCertificate(cert)
	}

	return &WindowsAgent{
		Hostname:      info.Hostname,
		Arch:          info.Architecture,
		BaseURL:       baseurl,
		AgentID:       agentid,
		ApiURL:        apiurl,
		Token:         token,
		AgentPK:       pk,
		Cert:          cert,
		ProgramDir:    pd,
		EXE:           exe,
		SystemDrive:   sd,
		Nssm:          nssm,
		MeshInstaller: mesh,
		MeshSystemEXE: filepath.Join(os.Getenv("ProgramFiles"), "Mesh Agent", "MeshAgent.exe"),
		MeshSVC:       "mesh agent",
		PyBin:         pybin,
		Headers:       headers,
		Logger:        logger,
		Version:       version,
		Debug:         logger.IsLevelEnabled(logrus.DebugLevel),
		rClient:       restyC,
	}
}

// ArchInfo returns architecture-specific filenames and URLs
func ArchInfo(programDir string) (nssm, mesh string) {
	switch runtime.GOARCH {
	case "amd64":
		nssm = filepath.Join(programDir, "nssm.exe")
		mesh = "meshagent.exe"
	case "386":
		nssm = filepath.Join(programDir, "nssm-x86.exe")
		mesh = "meshagent-x86.exe"
	}
	return
}

// OSInfo returns formatted OS names
func (a *WindowsAgent) OSInfo() (plat, osFullName string) {
	host, _ := ps.Host()
	info := host.Info()
	osInfo := info.OS

	var arch string
	switch info.Architecture {
	case "x86_64":
		arch = "64 bit"
	case "x86":
		arch = "32 bit"
	}

	plat = osInfo.Platform
	osFullName = fmt.Sprintf("%s, %s (build %s)", osInfo.Name, arch, osInfo.Build)
	return
}

// GetDisks returns a list of fixed disks
func (a *WindowsAgent) GetDisks() []rmm.Disk {
	ret := make([]rmm.Disk, 0)
	partitions, err := disk.Partitions(false)
	if err != nil {
		a.Logger.Debugln(err)
		return ret
	}

	for _, p := range partitions {
		typepath, _ := windows.UTF16PtrFromString(p.Device)
		typeval, _, _ := getDriveType.Call(uintptr(unsafe.Pointer(typepath)))
		// https://docs.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-getdrivetypea
		if typeval != 3 {
			continue
		}

		usage, err := disk.Usage(p.Mountpoint)
		if err != nil {
			a.Logger.Debugln(err)
			continue
		}

		d := rmm.Disk{
			Device:  p.Device,
			Fstype:  p.Fstype,
			Total:   usage.Total,
			Used:    usage.Used,
			Free:    usage.Free,
			Percent: usage.UsedPercent,
		}
		ret = append(ret, d)
	}
	return ret
}

// CMDShell mimics Python's `subprocess.run(shell=True)`
func CMDShell(shell string, cmdArgs []string, command string, timeout int, detached bool) (output [2]string, e error) {
	var (
		outb     bytes.Buffer
		errb     bytes.Buffer
		cmd      *exec.Cmd
		timedOut bool = false
	)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	if len(cmdArgs) > 0 && command == "" {
		switch shell {
		case "cmd":
			cmdArgs = append([]string{"/C"}, cmdArgs...)
			cmd = exec.Command("cmd.exe", cmdArgs...)
		case "powershell":
			cmdArgs = append([]string{"-NonInteractive", "-NoProfile"}, cmdArgs...)
			cmd = exec.Command("powershell.exe", cmdArgs...)
		}
	} else {
		switch shell {
		case "cmd":
			cmd = exec.Command("cmd.exe")
			cmd.SysProcAttr = &windows.SysProcAttr{
				CmdLine: fmt.Sprintf("cmd.exe /C %s", command),
			}
		case "powershell":
			cmd = exec.Command("Powershell", "-NonInteractive", "-NoProfile", command)
		}
	}

	// https://docs.microsoft.com/en-us/windows/win32/procthread/process-creation-flags
	if detached {
		cmd.SysProcAttr = &windows.SysProcAttr{
			CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
		}
	}
	cmd.Stdout = &outb
	cmd.Stderr = &errb
	err := cmd.Start()

	pid := int32(cmd.Process.Pid)

	go func(p int32) {

		<-ctx.Done()

		_ = KillProc(p)
		timedOut = true
	}(pid)

	err = cmd.Wait()

	if timedOut {
		return [2]string{outb.String(), errb.String()}, ctx.Err()
	}

	if err != nil {
		return [2]string{outb.String(), errb.String()}, err
	}

	return [2]string{outb.String(), errb.String()}, nil
}

// CMD runs a command with shell=False
func CMD(exe string, args []string, timeout int, detached bool) (output [2]string, e error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	var outb, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, exe, args...)
	if detached {
		cmd.SysProcAttr = &windows.SysProcAttr{
			CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
		}
	}
	cmd.Stdout = &outb
	cmd.Stderr = &errb
	err := cmd.Run()
	if err != nil {
		return [2]string{"", ""}, fmt.Errorf("%s: %s", err, errb.String())
	}

	if ctx.Err() == context.DeadlineExceeded {
		return [2]string{"", ""}, ctx.Err()
	}

	return [2]string{outb.String(), errb.String()}, nil
}

// EnablePing modifies the Windows Firewall ruleset to allow incoming ICMPv4
// todo: 2021-12-31: this may not always work, especially if enforced by a GPO (is this even needed?)
func EnablePing() {
	args := make([]string, 0)
	cmd := `netsh advfirewall firewall add rule name="ICMP Allow incoming V4 echo request" protocol=icmpv4:8,any dir=in action=allow`
	_, err := CMDShell("cmd", args, cmd, 10, false)
	if err != nil {
		fmt.Println(err)
	}
}

// EnableRDP enables Remote Desktop
// todo: 2021-12-31: this may not always work if enforced by a GPO
func EnableRDP() {
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Terminal Server`, registry.ALL_ACCESS)
	if err != nil {
		fmt.Println(err)
	}
	defer k.Close()

	err = k.SetDWordValue("fDenyTSConnections", 0)
	if err != nil {
		fmt.Println(err)
	}

	args := make([]string, 0)
	// todo: 2021-12-31: this may not always work if enforced by a GPO
	cmd := `netsh advfirewall firewall set rule group="remote desktop" new enable=Yes`
	_, cerr := CMDShell("cmd", args, cmd, 10, false)
	if cerr != nil {
		fmt.Println(cerr)
	}
}

// DisableSleepHibernate disables sleep and hibernate
func DisableSleepHibernate() {
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Session Manager\Power`, registry.ALL_ACCESS)
	if err != nil {
		fmt.Println(err)
	}
	defer k.Close()

	err = k.SetDWordValue("HiberbootEnabled", 0)
	if err != nil {
		fmt.Println(err)
	}

	args := make([]string, 0)

	var wg sync.WaitGroup
	currents := []string{"ac", "dc"}
	for _, i := range currents {
		wg.Add(1)
		go func(c string) {
			defer wg.Done()
			_, _ = CMDShell("cmd", args, fmt.Sprintf("powercfg /set%svalueindex scheme_current sub_buttons lidaction 0", c), 5, false)
			_, _ = CMDShell("cmd", args, fmt.Sprintf("powercfg /x -standby-timeout-%s 0", c), 5, false)
			_, _ = CMDShell("cmd", args, fmt.Sprintf("powercfg /x -hibernate-timeout-%s 0", c), 5, false)
			_, _ = CMDShell("cmd", args, fmt.Sprintf("powercfg /x -disk-timeout-%s 0", c), 5, false)
			_, _ = CMDShell("cmd", args, fmt.Sprintf("powercfg /x -monitor-timeout-%s 0", c), 5, false)
		}(i)
	}
	wg.Wait()
	_, _ = CMDShell("cmd", args, "powercfg -S SCHEME_CURRENT", 5, false)
}

// LoggedOnUser returns the first logged on user it finds
// todo: 2021-12-31: Python dep; replace with PowerShell?
func (a *WindowsAgent) LoggedOnUser() string {
	pyCode := `
import psutil

try:
	u = psutil.users()[0].name
	if u.isascii():
		print(u, end='')
	else:
		print('notascii', end='')
except Exception as e:
	print("None", end='')

`
	// Attempt #1: try with psutil first
	user, err := a.RunPythonCode(pyCode, 5, []string{})
	if err == nil && user != "notascii" {
		return user
	}

	// Attempt #2: Go fallback
	users, err := wapf.ListLoggedInUsers()
	if err != nil {
		a.Logger.Debugln("LoggedOnUser error", err)
		return "None"
	}

	if len(users) == 0 {
		return "None"
	}

	for _, u := range users {
		// Strip the 'Domain\' (or 'ComputerName\') prefix
		return strings.Split(u.FullUser(), `\`)[1]
	}
	return "None"
}

func (a *WindowsAgent) GetCPULoadAvg() int {
	// todo: 2021-12-31: remove Python dep
	fallback := false
	pyCode := `
import psutil
try:
	print(int(round(psutil.cpu_percent(interval=10))), end='')
except:
	print("pyerror", end='')
`
	pypercent, err := a.RunPythonCode(pyCode, 13, []string{})
	if err != nil || pypercent == "pyerror" {
		fallback = true
	}

	i, err := strconv.Atoi(pypercent)
	if err != nil {
		fallback = true
	}

	if fallback {
		percent, err := cpu.Percent(10*time.Second, false)
		if err != nil {
			a.Logger.Debugln("Go CPU Check:", err)
			return 0
		}
		return int(math.Round(percent[0]))
	}
	return i
}

// ForceKillSalt kills all salt related processes
func (a *WindowsAgent) ForceKillSalt() {
	pids := make([]int, 0)

	procs, err := ps.Processes()
	if err != nil {
		return
	}

	for _, process := range procs {
		p, err := process.Info()
		if err != nil {
			continue
		}
		if strings.ToLower(p.Name) == "python.exe" && strings.Contains(strings.ToLower(p.Exe), "salt") {
			pids = append(pids, p.PID)
		}
	}

	for _, pid := range pids {
		a.Logger.Debugln("Killing salt process with pid %d", pid)
		if err := KillProc(int32(pid)); err != nil {
			a.Logger.Debugln(err)
		}
	}
}

// ForceKillMesh kills all MeshAgent-related processes
func (a *WindowsAgent) ForceKillMesh() {
	pids := make([]int, 0)

	procs, err := ps.Processes()
	if err != nil {
		return
	}

	for _, process := range procs {
		p, err := process.Info()
		if err != nil {
			continue
		}
		if strings.Contains(strings.ToLower(p.Name), "meshagent") {
			pids = append(pids, p.PID)
		}
	}

	for _, pid := range pids {
		a.Logger.Debugln("Killing MeshAgent process with pid %d", pid)
		if err := KillProc(int32(pid)); err != nil {
			a.Logger.Debugln(err)
		}
	}
}

// RecoverTacticalAgent should only be called from the RPC service
func (a *WindowsAgent) RecoverTacticalAgent() {
	// todo: 2021-12-31: custom branding?
	svc := "tacticalagent"
	a.Logger.Debugln("Attempting TacticalAgent recovery on", a.Hostname)
	defer CMD(a.Nssm, []string{"start", svc}, 60, false)

	_, _ = CMD(a.Nssm, []string{"stop", svc}, 120, false)
	_, _ = CMD("ipconfig", []string{"/flushdns"}, 15, false)
	a.Logger.Debugln("TacticalAgent recovery completed on", a.Hostname)
}

// RecoverSalt recovers the salt minion
func (a *WindowsAgent) RecoverSalt() {
	saltSVC := "salt-minion"
	a.Logger.Debugln("Attempting salt recovery on", a.Hostname)
	defer CMD(a.Nssm, []string{"start", saltSVC}, 60, false)

	_, _ = CMD(a.Nssm, []string{"stop", saltSVC}, 120, false)
	a.ForceKillSalt()
	time.Sleep(2 * time.Second)
	cacheDir := filepath.Join(a.SystemDrive, "\\salt", "var", "cache", "salt", "minion")
	a.Logger.Debugln("Clearing salt cache in", cacheDir)
	err := os.RemoveAll(cacheDir)
	if err != nil {
		a.Logger.Debugln(err)
	}
	_, _ = CMD("ipconfig", []string{"/flushdns"}, 15, false)
	a.Logger.Debugln("Salt recovery completed on", a.Hostname)
}

func (a *WindowsAgent) SyncMeshNodeID() {
	out, err := CMD(a.MeshSystemEXE, []string{"-nodeid"}, 10, false)
	if err != nil {
		a.Logger.Debugln(err)
		return
	}

	stdout := out[0]
	stderr := out[1]

	if stderr != "" {
		a.Logger.Debugln(stderr)
		return
	}

	if stdout == "" || strings.Contains(strings.ToLower(StripAll(stdout)), "not defined") {
		a.Logger.Debugln("Failed getting Mesh Node ID", stdout)
		return
	}

	// 2021-12-31: api/tacticalrmm/apiv3/views.py:94
	payload := rmm.MeshNodeID{
		Func:    "syncmesh",
		Agentid: a.AgentID,
		NodeID:  StripAll(stdout),
	}

	_, err = a.rClient.R().SetBody(payload).Post("/api/v3/syncmesh/")
	if err != nil {
		a.Logger.Debugln("SyncMesh:", err)
	}
}

// RecoverMesh Recovers the MeshAgent service
func (a *WindowsAgent) RecoverMesh() {
	a.Logger.Infoln("Attempting MeshAgent service recovery")
	defer CMD("net", []string{"start", a.MeshSVC}, 60, false)

	_, _ = CMD("net", []string{"stop", a.MeshSVC}, 60, false)
	a.ForceKillMesh()
	a.SyncMeshNodeID()
}

// RecoverRPC Recovers the NATS RPC service
func (a *WindowsAgent) RecoverRPC() {
	a.Logger.Infoln("Attempting RPC service recovery")
	_, _ = CMD("net", []string{"stop", "tacticalrpc"}, 90, false)
	time.Sleep(2 * time.Second)
	_, _ = CMD("net", []string{"start", "tacticalrpc"}, 90, false)
}

// RecoverCMD runs a shell recovery command
func (a *WindowsAgent) RecoverCMD(command string) {
	a.Logger.Infoln("Attempting shell recovery with command:", command)
	// To prevent killing ourselves, prefix the command with 'cmd /C'
	// so the parent process is now cmd.exe and not tacticalrmm.exe
	cmd := exec.Command("cmd.exe")
	cmd.SysProcAttr = &windows.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
		CmdLine:       fmt.Sprintf("cmd.exe /C %s", command), // properly escape in case double quotes are in the command
	}
	cmd.Start()
}

func (a *WindowsAgent) Sync() {
	a.GetWMI()
	time.Sleep(1 * time.Second)
	a.SendSoftware()
}

func (a *WindowsAgent) SendSoftware() {
	sw := a.GetInstalledSoftware()
	a.Logger.Debugln(sw)

	payload := map[string]interface{}{"agent_id": a.AgentID, "software": sw}

	// 2021-12-31: api/tacticalrmm/apiv3/views.py:461
	_, err := a.rClient.R().SetBody(payload).Post("/api/v3/software/")
	if err != nil {
		a.Logger.Debugln(err)
	}
}

func (a *WindowsAgent) UninstallCleanup() {
	registry.DeleteKey(registry.LOCAL_MACHINE, `SOFTWARE\TacticalRMM`)
	a.CleanupAgentUpdates()
	CleanupSchedTasks()
}

// ShowStatus prints the Windows service status
// If called from an interactive desktop, pops up a message box
// Otherwise prints to the console
func ShowStatus(version string) {
	statusMap := make(map[string]string)
	svcs := []string{"tacticalagent", "tacticalrpc", "mesh agent"}

	for _, service := range svcs {
		status, err := GetServiceStatus(service)
		if err != nil {
			statusMap[service] = "Not Installed"
			continue
		}
		statusMap[service] = status
	}

	window := w32.GetForegroundWindow()
	if window != 0 {
		_, consoleProcID := w32.GetWindowThreadProcessId(window)
		if w32.GetCurrentProcessId() == consoleProcID {
			w32.ShowWindow(window, w32.SW_HIDE)
		}
		var handle w32.HWND
		msg := fmt.Sprintf("Agent: %s\n\nRPC Service: %s\n\nMesh Agent: %s", statusMap["tacticalagent"], statusMap["tacticalrpc"], statusMap["mesh agent"])
		w32.MessageBox(handle, msg, fmt.Sprintf("Tactical RMM v%s", version), w32.MB_OK|w32.MB_ICONINFORMATION)
	} else {
		// todo: 2021-12-31: custom branding
		fmt.Println("Tactical RMM Version", version)
		fmt.Println("Agent:", statusMap["tacticalagent"])
		fmt.Println("RPC Service:", statusMap["tacticalrpc"])
		fmt.Println("Mesh Agent:", statusMap["mesh agent"])
	}
}

func (a *WindowsAgent) installerMsg(msg, alert string, silent bool) {
	window := w32.GetForegroundWindow()
	if !silent && window != 0 {
		var (
			handle w32.HWND
			flags  uint
		)

		switch alert {
		case "info":
			flags = w32.MB_OK | w32.MB_ICONINFORMATION
		case "error":
			flags = w32.MB_OK | w32.MB_ICONERROR
		default:
			flags = w32.MB_OK | w32.MB_ICONINFORMATION
		}

		w32.MessageBox(handle, msg, "Tactical RMM", flags)
	} else {
		fmt.Println(msg)
	}

	if alert == "error" {
		a.Logger.Fatalln(msg)
	}
}

func (a *WindowsAgent) AgentUpdate(url, inno, version string) {
	time.Sleep(time.Duration(randRange(1, 15)) * time.Second)
	a.CleanupAgentUpdates()
	updater := filepath.Join(a.ProgramDir, inno)
	a.Logger.Infof("Agent updating from %s to %s", a.Version, version)
	a.Logger.Infoln("Downloading agent update from", url)

	rClient := resty.New()
	rClient.SetCloseConnection(true)
	rClient.SetTimeout(15 * time.Minute)
	rClient.SetDebug(a.Debug)
	r, err := rClient.R().SetOutput(updater).Get(url)
	if err != nil {
		a.Logger.Errorln(err)
		CMD("net", []string{"start", "tacticalrpc"}, 10, false)
		return
	}
	if r.IsError() {
		a.Logger.Errorln("Download failed with status code", r.StatusCode())
		CMD("net", []string{"start", "tacticalrpc"}, 10, false)
		return
	}

	dir, err := ioutil.TempDir("", "tacticalrmm")
	if err != nil {
		a.Logger.Errorln("AgentUpdate create tempdir:", err)
		CMD("net", []string{"start", "tacticalrpc"}, 10, false)
		return
	}

	// todo: 2021-12-31: custom branding
	innoLogFile := filepath.Join(dir, "tacticalrmm.txt")

	args := []string{"/C", updater, "/VERYSILENT", fmt.Sprintf("/LOG=%s", innoLogFile)}
	cmd := exec.Command("cmd.exe", args...)
	cmd.SysProcAttr = &windows.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
	cmd.Start()
	time.Sleep(1 * time.Second)
}

func (a *WindowsAgent) setupNatsOptions() []nats.Option {
	opts := make([]nats.Option, 0)
	opts = append(opts, nats.Name("TacticalRMM"))
	opts = append(opts, nats.UserInfo(a.AgentID, a.Token))
	opts = append(opts, nats.ReconnectWait(time.Second*5))
	opts = append(opts, nats.RetryOnFailedConnect(true))
	opts = append(opts, nats.MaxReconnects(-1))
	opts = append(opts, nats.ReconnectBufSize(-1))
	return opts
}

func (a *WindowsAgent) GetUninstallExe() string {
	cderr := os.Chdir(a.ProgramDir)
	if cderr == nil {
		files, err := filepath.Glob("unins*.exe")
		if err == nil {
			for _, f := range files {
				if strings.Contains(f, "001") {
					return f
				}
			}
		}
	}
	return "unins000.exe"
}

func (a *WindowsAgent) AgentUninstall() {
	tacUninst := filepath.Join(a.ProgramDir, a.GetUninstallExe())
	args := []string{"/C", tacUninst, "/VERYSILENT", "/SUPPRESSMSGBOXES", "/FORCECLOSEAPPLICATIONS"}
	cmd := exec.Command("cmd.exe", args...)
	cmd.SysProcAttr = &windows.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
	cmd.Start()
}

func (a *WindowsAgent) CleanupAgentUpdates() {
	cderr := os.Chdir(a.ProgramDir)
	if cderr != nil {
		a.Logger.Errorln(cderr)
		return
	}

	files, err := filepath.Glob("winagent-v*.exe")
	if err == nil {
		for _, f := range files {
			os.Remove(f)
		}
	}

	cderr = os.Chdir(os.Getenv("TMP"))
	if cderr != nil {
		a.Logger.Errorln(cderr)
		return
	}
	folders, err := filepath.Glob("tacticalrmm*")
	if err == nil {
		for _, f := range folders {
			os.RemoveAll(f)
		}
	}
}

func (a *WindowsAgent) RunPythonCode(code string, timeout int, args []string) (string, error) {
	content := []byte(code)
	dir, err := ioutil.TempDir("", "tacticalpy")
	if err != nil {
		a.Logger.Debugln(err)
		return "", err
	}
	defer os.RemoveAll(dir)

	tmpfn, _ := ioutil.TempFile(dir, "*.py")
	if _, err := tmpfn.Write(content); err != nil {
		a.Logger.Debugln(err)
		return "", err
	}
	if err := tmpfn.Close(); err != nil {
		a.Logger.Debugln(err)
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	var outb, errb bytes.Buffer
	cmdArgs := []string{tmpfn.Name()}
	if len(args) > 0 {
		cmdArgs = append(cmdArgs, args...)
	}
	a.Logger.Debugln(cmdArgs)
	cmd := exec.CommandContext(ctx, a.PyBin, cmdArgs...)
	cmd.Stdout = &outb
	cmd.Stderr = &errb

	cmdErr := cmd.Run()

	if ctx.Err() == context.DeadlineExceeded {
		a.Logger.Debugln("RunPythonCode:", ctx.Err())
		return "", ctx.Err()
	}

	if cmdErr != nil {
		a.Logger.Debugln("RunPythonCode:", cmdErr)
		return "", cmdErr
	}

	if errb.String() != "" {
		a.Logger.Debugln(errb.String())
		return errb.String(), errors.New("RunPythonCode stderr")
	}

	return outb.String(), nil

}

// GetPython download Python
// todo: 2021-12-31: make Python *optional*
func (a *WindowsAgent) GetPython(force bool) {
	if FileExists(a.PyBin) && !force {
		return
	}

	var archZip string
	var folder string
	switch runtime.GOARCH {
	case "amd64":
		archZip = "py38-x64.zip"
		folder = "py38-x64"
	case "386":
		archZip = "py38-x32.zip"
		folder = "py38-x32"
	}
	pyFolder := filepath.Join(a.ProgramDir, folder)
	pyZip := filepath.Join(a.ProgramDir, archZip)
	a.Logger.Debugln(pyZip)
	a.Logger.Debugln(a.PyBin)
	defer os.Remove(pyZip)

	if force {
		os.RemoveAll(pyFolder)
	}

	rClient := resty.New()
	rClient.SetTimeout(20 * time.Minute)
	rClient.SetRetryCount(10)
	rClient.SetRetryWaitTime(1 * time.Minute)
	rClient.SetRetryMaxWaitTime(15 * time.Minute)

	// useAlternative := false

	// todo: 2021-12-28: we'll implement a better way of doing this later on
	url := fmt.Sprintf("https://localhost/%s/%s", a.Version, archZip)
	// nope
	// url2 := fmt.Sprintf("https://files.tacticalrmm.io/%s", archZip)
	a.Logger.Debugln(url)
	r, err := rClient.R().SetOutput(pyZip).Get(url)
	if err != nil {
		a.Logger.Errorln("Unable to download py3.zip from github, using alternative link.", err)
		// useAlternative = true
	}
	if r.IsError() {
		a.Logger.Errorln("Unable to download py3.zip from github, using alternative link. Status code", r.StatusCode())
		// useAlternative = true
	}

	/*if useAlternative {
		a.Logger.Debugln(url2)
		r1, err := rClient.R().SetOutput(pyZip).Get(url2)
		if err != nil {
			a.Logger.Errorln("Unable to download py3.zip:", err)
			return
		}
		if r1.IsError() {
			a.Logger.Errorln("Unable to download py3.zip. Status code", r.StatusCode())
			return
		}
	}*/

	err = Unzip(pyZip, a.ProgramDir)
	if err != nil {
		a.Logger.Errorln(err)
	}
}

func (a *WindowsAgent) RemoveSalt() error {
	saltFiles := []string{"saltcustom", "salt-minion-setup.exe", "salt-minion-setup-x86.exe"}
	for _, sf := range saltFiles {
		if FileExists(filepath.Join(a.ProgramDir, sf)) {
			os.Remove(filepath.Join(a.ProgramDir, sf))
		}
	}

	saltUnins := filepath.Join(a.SystemDrive, "\\salt", "uninst.exe")
	if !FileExists(saltUnins) {
		return errors.New("salt uninstaller does not exist")
	}

	_, err := CMD(saltUnins, []string{"/S"}, 900, false)
	if err != nil {
		a.Logger.Debugln("Error uninstalling salt:", err)
		return errors.New(err.Error())
	}
	return nil
}

func (a *WindowsAgent) deleteOldTacticalServices() {
	services := []string{"checkrunner"}
	for _, svc := range services {
		if serviceExists(svc) {
			_, _ = CMD(a.Nssm, []string{"stop", svc}, 30, false)
			_, _ = CMD(a.Nssm, []string{"remove", svc, "confirm"}, 30, false)
		}
	}
}

func (a *WindowsAgent) addDefenderExlusions() {
	// todo: 2021-12-31: make this *optional*
	code := `
Add-MpPreference -ExclusionPath 'C:\Program Files\TacticalAgent\*'
Add-MpPreference -ExclusionPath 'C:\Windows\Temp\winagent-v*.exe'
Add-MpPreference -ExclusionPath 'C:\Windows\Temp\trmm\*'
Add-MpPreference -ExclusionPath 'C:\Program Files\Mesh Agent\*'
`
	_, _, _, err := a.RunScript(code, "powershell", []string{}, 20)
	if err != nil {
		a.Logger.Debugln(err)
	}
}

// RunMigrations cleans up unused stuff from older agents
func (a *WindowsAgent) RunMigrations() {
	a.deleteOldTacticalServices()
	CMD("schtasks.exe", []string{"/delete", "/TN", "TacticalRMM_fixmesh", "/f"}, 10, false)
}

func (a *WindowsAgent) CheckForRecovery() {
	url := fmt.Sprintf("/api/v3/%s/recovery/", a.AgentID)
	r, err := a.rClient.R().SetResult(&rmm.RecoveryAction{}).Get(url)

	if err != nil {
		a.Logger.Debugln("Recovery:", err)
		return
	}
	if r.IsError() {
		a.Logger.Debugln("Recovery status code:", r.StatusCode())
		return
	}

	mode := r.Result().(*rmm.RecoveryAction).Mode
	command := r.Result().(*rmm.RecoveryAction).ShellCMD

	switch mode {
	// 2021-12-31: api/tacticalrmm/apiv3/views.py:551
	case "mesh":
		a.RecoverMesh()
	case "rpc":
		a.RecoverRPC()
	case "command":
		a.RecoverCMD(command)
	default:
		return
	}
}

func (a *WindowsAgent) CreateTRMMTempDir() {
	// Create the temp dir for running scripts
	// This can be 'C:\Windows\Temp\trmm\' or '\AppData\Local\Temp\trmm'
	dir := filepath.Join(os.TempDir(), "trmm")
	if !FileExists(dir) {
		// todo: 2021-12-31: verify permissions
		err := os.Mkdir(dir, 0775)
		if err != nil {
			a.Logger.Errorln(err)
		}
	}
}
