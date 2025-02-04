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
	"github.com/sarog/trmm-shared"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	getDriveType = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetDriveTypeW")
)

const (
	// todo: 2022-01-01: consolidate these elsewhere
	AGENT_FOLDER        = "RMMAgent"
	API_URL_SOFTWARE    = "/api/v3/software/"
	API_URL_SYNCMESH    = "/api/v3/syncmesh/"
	AGENT_NAME_LONG     = "RMM Agent"
	AGENT_TEMP_DIR      = "rmm"
	AGENT_FILENAME      = "rmmagent.exe"
	INNO_SETUP_DIR      = "rmmagent"
	INNO_SETUP_LOGFILE  = "rmmagent.txt"
	NATS_RMM_IDENTIFIER = "ACMERMM"
	NATS_DEFAULT_PORT   = 4222
	RMM_SEARCH_PREFIX   = "acmermm*"
	PYTHON_TEMP_DIR     = "rmmagentpy"
	MESH_AGENT_FOLDER   = "Mesh Agent"
	MESH_AGENT_FILENAME = "MeshAgent.exe"
	MESH_AGENT_NAME     = "meshagent"

	AGENT_MODE_MESH    = "mesh"
	AGENT_MODE_COMMAND = "command"
)

// Agent struct
// 2022-01-01: renamed to 'Agent' from 'WindowsAgent'
type Agent struct {
	Hostname      string
	Arch          string
	AgentID       string
	BaseURL       string
	ApiURL        string
	ApiPort       int
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
	PythonEnabled bool
	PythonBinary  string
	Headers       map[string]string
	Logger        *logrus.Logger
	Version       string
	Debug         bool
	rClient       *resty.Client
}

// New Initializes a new Agent with logger
func New(logger *logrus.Logger, version string) *Agent {
	host, _ := ps.Host()
	info := host.Info()
	pd := filepath.Join(os.Getenv("ProgramFiles"), AGENT_FOLDER)
	exe := filepath.Join(pd, AGENT_FILENAME)
	dbFile := filepath.Join(pd, "agentdb.db") // Deprecated
	sd := os.Getenv("SystemDrive")
	nssm, mesh := ArchInfo(pd)

	var pyBin string
	switch runtime.GOARCH {
	case "amd64":
		pyBin = filepath.Join(pd, "py38-x64", "python.exe")
	case "386":
		pyBin = filepath.Join(pd, "py38-x32", "python.exe")
	}

	// Previous Python agent database
	if FileExists(dbFile) {
		os.Remove(dbFile)
	}

	var (
		baseurl   string
		agentid   string
		apiurl    string
		token     string
		agentpk   string
		pk        int
		cert      string
		pyStr     string
		pyEnabled bool
	)

	// todo: 2021-12-31: migrate to DPAPI?
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, REG_RMM_PATH, registry.ALL_ACCESS)
	if err == nil {
		baseurl, _, err = key.GetStringValue(REG_RMM_BASEURL)
		if err != nil {
			logger.Fatalln("Unable to get BaseURL:", err)
		}

		agentid, _, err = key.GetStringValue(REG_RMM_AGENTID)
		if err != nil {
			logger.Fatalln("Unable to get AgentID:", err)
		}

		apiurl, _, err = key.GetStringValue(REG_RMM_APIURL)
		if err != nil {
			logger.Fatalln("Unable to get ApiURL:", err)
		}

		token, _, err = key.GetStringValue(REG_RMM_TOKEN)
		if err != nil {
			logger.Fatalln("Unable to get Token:", err)
		}

		agentpk, _, err = key.GetStringValue(REG_RMM_AGENTPK)
		if err != nil {
			logger.Fatalln("Unable to get AgentPK:", err)
		}

		pk, _ = strconv.Atoi(agentpk)

		cert, _, _ = key.GetStringValue(REG_RMM_CERT)

		pyStr = "false"
		pyStr, _, err = key.GetStringValue(REG_RMM_PYENABLED)
		if err != nil {
			logger.Warnln("Unable to get PythonEnabled:", err)
			key.SetStringValue(REG_RMM_PYENABLED, "false")
		}
		pyEnabled, _ = strconv.ParseBool(pyStr)
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

	return &Agent{
		Hostname:      info.Hostname,
		Arch:          info.Architecture,
		BaseURL:       baseurl,
		AgentID:       agentid,
		ApiURL:        apiurl,
		ApiPort:       NATS_DEFAULT_PORT,
		Token:         token,
		AgentPK:       pk,
		Cert:          cert,
		ProgramDir:    pd,
		EXE:           exe,
		SystemDrive:   sd,
		Nssm:          nssm,
		MeshInstaller: mesh,
		MeshSystemEXE: filepath.Join(os.Getenv("ProgramFiles"), MESH_AGENT_FOLDER, MESH_AGENT_FILENAME),
		MeshSVC:       SERVICE_NAME_MESHAGENT,
		PythonBinary:  pyBin,
		PythonEnabled: pyEnabled,
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
func (a *Agent) OSInfo() (plat, osFullName string) {
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

// GetDisksNATS returns a list of fixed disks
func (a *Agent) GetDisksNATS() []trmm.Disk {
	ret := make([]trmm.Disk, 0)
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

		d := trmm.Disk{
			Device:  p.Device,
			Fstype:  p.Fstype,
			Total:   string(usage.Total),
			Used:    string(usage.Used),
			Free:    string(usage.Free),
			Percent: int(usage.UsedPercent),
		}
		ret = append(ret, d)
	}
	return ret
}

// GetDisks returns a list of fixed disks
// Deprecated
func (a *Agent) GetDisks() []rmm.Disk {
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

// CMDShell Mimics Python's `subprocess.run(shell=True)`
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
	cmd := `netsh advfirewall firewall set rule group="Remote Desktop" new enable=Yes`
	_, cerr := CMDShell("cmd", args, cmd, 10, false)
	if cerr != nil {
		fmt.Println(cerr)
	}
}

// DisableSleepHibernate disables sleep and hibernate
// todo: 2023-04-17: see if the device is a laptop
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
func (a *Agent) LoggedOnUser() string {

	// 2022-01-02: Works in PowerShell 5.x and Core 7.x
	cmd := "((Get-CimInstance -ClassName Win32_ComputerSystem).Username).Split('\\')[1]"
	user, _, _, err := a.RunScript(cmd, "powershell", []string{}, 20)
	if err != nil {
		a.Logger.Debugln(err)
	}
	if err == nil {
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

// GetCPULoadAvg Retrieve CPU load average
func (a *Agent) GetCPULoadAvg() int {
	fallback := false

	// 2022-01-02: Works in PowerShell 5.x and Core 7.x
	// todo? | Measure-Object -Property LoadPercentage -Average | Select Average
	cmd := "(Get-CimInstance -ClassName Win32_Processor).LoadPercentage"
	load, _, _, err := a.RunScript(cmd, "powershell", []string{}, 20)

	if err != nil {
		a.Logger.Debugln(err)
		fallback = true
	}

	i, _ := strconv.Atoi(load)

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
// Deprecated
func (a *Agent) ForceKillSalt() {
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
func (a *Agent) ForceKillMesh() {
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
		if strings.Contains(strings.ToLower(p.Name), MESH_AGENT_NAME) {
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

// RecoverAgent Recover the Agent; only called from the RPC service
func (a *Agent) RecoverAgent() {
	a.Logger.Debugln("Attempting ", AGENT_NAME_LONG, " recovery on", a.Hostname)
	defer CMD(a.Nssm, []string{"start", SERVICE_NAME_AGENT}, 60, false)
	_, _ = CMD(a.Nssm, []string{"stop", SERVICE_NAME_AGENT}, 120, false)
	_, _ = CMD("ipconfig", []string{"/flushdns"}, 15, false)
	a.Logger.Debugln(AGENT_NAME_LONG, " recovery completed on", a.Hostname)
}

// RecoverSalt recovers the salt minion
// Deprecated
func (a *Agent) RecoverSalt() {
	a.Logger.Debugln("Attempting salt recovery on", a.Hostname)
	defer CMD(a.Nssm, []string{"start", SERVICE_NAME_SALTMINION}, 60, false)
	_, _ = CMD(a.Nssm, []string{"stop", SERVICE_NAME_SALTMINION}, 120, false)
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

func (a *Agent) SyncMeshNodeID() {
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

	_, err = a.rClient.R().SetBody(payload).Post(API_URL_SYNCMESH)
	if err != nil {
		a.Logger.Debugln("SyncMesh:", err)
	}
}

// RecoverMesh Recovers the MeshAgent service
func (a *Agent) RecoverMesh() {
	a.Logger.Infoln("Attempting MeshAgent service recovery")
	defer CMD("net", []string{"start", a.MeshSVC}, 60, false)
	_, _ = CMD("net", []string{"stop", a.MeshSVC}, 60, false)
	a.ForceKillMesh()
	a.SyncMeshNodeID()
}

// RecoverRPC Recovers the NATS RPC service
func (a *Agent) RecoverRPC() {
	a.Logger.Infoln("Attempting RPC service recovery")
	_, _ = CMD("net", []string{"stop", SERVICE_NAME_RPC}, 90, false)
	time.Sleep(2 * time.Second)
	_, _ = CMD("net", []string{"start", SERVICE_NAME_RPC}, 90, false)
}

// RecoverCMD runs a shell recovery command
func (a *Agent) RecoverCMD(command string) {
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

func (a *Agent) Sync() {
	a.GetWMI()
	time.Sleep(1 * time.Second)
	a.SendSoftware()
}

// SendSoftware Send list of installed software
func (a *Agent) SendSoftware() {
	sw := a.GetInstalledSoftware()
	a.Logger.Debugln(sw)

	payload := map[string]interface{}{
		"agent_id": a.AgentID,
		"software": sw,
	}

	// 2021-12-31: api/tacticalrmm/apiv3/views.py:461
	_, err := a.rClient.R().SetBody(payload).Post(API_URL_SOFTWARE)
	if err != nil {
		a.Logger.Debugln(err)
	}
}

func (a *Agent) UninstallCleanup() {
	registry.DeleteKey(registry.LOCAL_MACHINE, REG_RMM_PATH)
	a.CleanupAgentUpdates()
	CleanupSchedTasks()
}

// ShowStatus prints the Windows service status
// If called from an interactive desktop, pops up a message box
// Otherwise prints to the console
func ShowStatus(version string) {
	statusMap := make(map[string]string)
	svcs := []string{SERVICE_NAME_AGENT, SERVICE_NAME_RPC, SERVICE_NAME_MESHAGENT}

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
		msg := fmt.Sprintf("Agent: %s\n\nRPC Service: %s\n\nMesh Agent: %s",
			statusMap[SERVICE_NAME_AGENT], statusMap[SERVICE_NAME_RPC], statusMap[SERVICE_NAME_MESHAGENT])

		w32.MessageBox(handle, msg, fmt.Sprintf("RMM Agent v%s", version), w32.MB_OK|w32.MB_ICONINFORMATION)
	} else {
		fmt.Println("RMM Version", version)
		fmt.Println("Agent Service:", statusMap[SERVICE_NAME_AGENT])
		fmt.Println("RPC Service:", statusMap[SERVICE_NAME_RPC])
		fmt.Println("Mesh Agent:", statusMap[SERVICE_NAME_MESHAGENT])
	}
}

func (a *Agent) installerMsg(msg, alert string, silent bool) {
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

		w32.MessageBox(handle, msg, AGENT_NAME_LONG, flags)
	} else {
		fmt.Println(msg)
	}

	if alert == "error" {
		a.Logger.Fatalln(msg)
	}
}

func (a *Agent) AgentUpdate(url, inno, version string) {
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
		CMD("net", []string{"start", SERVICE_NAME_RPC}, 10, false)
		return
	}
	if r.IsError() {
		a.Logger.Errorln("Download failed with status code", r.StatusCode())
		CMD("net", []string{"start", SERVICE_NAME_RPC}, 10, false)
		return
	}

	dir, err := ioutil.TempDir("", INNO_SETUP_DIR)
	if err != nil {
		a.Logger.Errorln("AgentUpdate unable to create temporary directory:", err)
		CMD("net", []string{"start", SERVICE_NAME_RPC}, 10, false)
		return
	}

	innoLogFile := filepath.Join(dir, INNO_SETUP_LOGFILE)

	args := []string{"/C", updater, "/VERYSILENT", fmt.Sprintf("/LOG=%s", innoLogFile)}
	cmd := exec.Command("cmd.exe", args...)
	cmd.SysProcAttr = &windows.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
	cmd.Start()
	time.Sleep(1 * time.Second)
}

func (a *Agent) setupNatsOptions() []nats.Option {
	opts := make([]nats.Option, 0)
	opts = append(opts, nats.Name(NATS_RMM_IDENTIFIER))
	opts = append(opts, nats.UserInfo(a.AgentID, a.Token))
	opts = append(opts, nats.ReconnectWait(time.Second*5))
	opts = append(opts, nats.RetryOnFailedConnect(true))
	opts = append(opts, nats.MaxReconnects(-1))
	opts = append(opts, nats.ReconnectBufSize(-1))
	return opts
}

func (a *Agent) GetUninstallExe() string {
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

func (a *Agent) AgentUninstall() {
	agentUninst := filepath.Join(a.ProgramDir, a.GetUninstallExe())
	args := []string{"/C", agentUninst, "/VERYSILENT", "/SUPPRESSMSGBOXES", "/FORCECLOSEAPPLICATIONS"}
	cmd := exec.Command("cmd.exe", args...)
	cmd.SysProcAttr = &windows.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
	cmd.Start()
}

func (a *Agent) CleanupAgentUpdates() {
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
	folders, err := filepath.Glob(RMM_SEARCH_PREFIX)
	if err == nil {
		for _, f := range folders {
			os.RemoveAll(f)
		}
	}
}

// RunPythonCode Run Python Code
func (a *Agent) RunPythonCode(code string, timeout int, args []string) (string, error) {
	if !a.PythonEnabled {
		a.Logger.Warnln("Python is disabled on this agent instance, skipping execution.")
		return "", errors.New("RunPythonCode disabled")
	}

	content := []byte(code)
	dir, err := ioutil.TempDir("", PYTHON_TEMP_DIR)
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
	cmd := exec.CommandContext(ctx, a.PythonBinary, cmdArgs...)
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

func (a *Agent) IsPythonInstalled() bool {
	if FileExists(a.PythonBinary) {
		return true
	}
	return false
}

// GetPython Download Python
// todo: 2023-04-17: remove
func (a *Agent) GetPython(force bool) {
	// 2022-01-02
	if !a.PythonEnabled {
		a.Logger.Debugln("Python is disabled on this agent instance, skipping installation.")
		return
	}

	if a.IsPythonInstalled() && !force {
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
	a.Logger.Debugln(a.PythonBinary)
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

// Deprecated
func (a *Agent) RemoveSalt() error {
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

// Deprecated
func (a *Agent) deleteOldAgentServices() {
	services := []string{"checkrunner"}
	for _, svc := range services {
		if serviceExists(svc) {
			_, _ = CMD(a.Nssm, []string{"stop", svc}, 30, false)
			_, _ = CMD(a.Nssm, []string{"remove", svc, "confirm"}, 30, false)
		}
	}
}

// todo: 2023-04-17: remove
func (a *Agent) addDefenderExclusions() {
	code := `
Add-MpPreference -ExclusionPath 'C:\Program Files\` + AGENT_NAME_LONG + `\*'
Add-MpPreference -ExclusionPath 'C:\Windows\Temp\winagent-v*.exe'
Add-MpPreference -ExclusionPath 'C:\Windows\Temp\trmm\*'
Add-MpPreference -ExclusionPath 'C:\Program Files\Mesh Agent\*'
`
	// todo: 2022-01-02: toggle add/remove via boolean
	// code := `
	// Remove-MpPreference -ExclusionPath 'C:\Program Files\`+AGENT_NAME_LONG+`\*'
	// Remove-MpPreference -ExclusionPath 'C:\Windows\Temp\winagent-v*.exe'
	// Remove-MpPreference -ExclusionPath 'C:\Windows\Temp\trmm\*'
	// Remove-MpPreference -ExclusionPath 'C:\Program Files\Mesh Agent\*'
	// `
	_, _, _, err := a.RunScript(code, "powershell", []string{}, 20)
	if err != nil {
		a.Logger.Debugln(err)
	}
}

// RunMigrations cleans up unused stuff from older agents
func (a *Agent) RunMigrations() {
	a.deleteOldAgentServices()
	CMD("schtasks.exe", []string{"/delete", "/TN", "RMM_fixmesh", "/f"}, 10, false)
}

// CheckForRecovery Check for agent recovery
// 2022-01-01: api/tacticalrmm/apiv3/urls.py:22
func (a *Agent) CheckForRecovery() {
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
	case AGENT_MODE_MESH:
		// 2022-01-01:
		// 	api/tacticalrmm/agents/views.py:236
		// 	api/tacticalrmm/agents/views.py:569
		a.RecoverMesh()
	case AGENT_MODE_RPC:
		a.RecoverRPC()
	case AGENT_MODE_COMMAND:
		// 2022-01-01: api/tacticalrmm/apiv3/views.py:552
		a.RecoverCMD(command)
	default:
		return
	}
}

// CreateAgentTempDir Create the temp directory for running scripts
// This can be 'C:\Windows\Temp\trmm\' or '\AppData\Local\Temp\trmm' depending on context
func (a *Agent) CreateAgentTempDir() {
	dir := filepath.Join(os.TempDir(), AGENT_TEMP_DIR)
	if !FileExists(dir) {
		// todo: 2021-12-31: verify permissions
		err := os.Mkdir(dir, 0775)
		if err != nil {
			a.Logger.Errorln(err)
		}
	}
}
