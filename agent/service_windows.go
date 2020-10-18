package agent

import (
	"encoding/json"
	"math/rand"
	"time"
)

//HelloPost post
type HelloPost struct {
	Agentid     string  `json:"agent_id"`
	Hostname    string  `json:"hostname"`
	OS          string  `json:"operating_system"`
	TotalRAM    float64 `json:"total_ram"`
	Platform    string  `json:"plat"`
	Version     string  `json:"version"`
	BootTime    int64   `json:"boot_time"`
	SaltVersion string  `json:"salt_ver"`
}

//HelloPatch patch
type HelloPatch struct {
	Agentid  string           `json:"agent_id"`
	Services []WindowsService `json:"services"`
	PublicIP string           `json:"public_ip"`
	Disks    []Disk           `json:"disks"`
	Username string           `json:"logged_in_username"`
	Version  string           `json:"version"`
	BootTime int64            `json:"boot_time"`
}

// WinAgentSvc tacticalagent windows nssm service
func (a *WindowsAgent) WinAgentSvc() {
	a.Logger.Infoln("Agent service started")
	a.CleanupPythonAgent()
	var data map[string]interface{}
	var sleep int

	url := a.Server + "/api/v3/hello/"
	req := &APIRequest{
		URL:       url,
		Headers:   a.Headers,
		Timeout:   15,
		LocalCert: a.DB.Cert,
		Debug:     a.Debug,
	}

	plat, osinfo := OSInfo()

	postPayload := HelloPost{
		Agentid:     a.AgentID,
		Hostname:    a.Hostname,
		OS:          osinfo,
		TotalRAM:    TotalRAM(),
		Platform:    plat,
		Version:     a.Version,
		BootTime:    BootTime(),
		SaltVersion: a.GetProgramVersion("salt minion"),
	}

	req.Method = "POST"
	req.Payload = postPayload
	a.Logger.Debugln(req)

	_, err := req.MakeRequest()
	if err != nil {
		a.Logger.Debugln(err)
	}

	time.Sleep(3 * time.Second)

	for {
		patchPayload := HelloPatch{
			Agentid:  a.AgentID,
			Services: a.GetServices(),
			PublicIP: PublicIP(),
			Disks:    a.GetDisks(),
			Username: LoggedOnUser(),
			Version:  a.Version,
			BootTime: BootTime(),
		}

		req.Method = "PATCH"
		req.Payload = patchPayload
		a.Logger.Debugln(req)

		r, err := req.MakeRequest()
		if err != nil {
			a.Logger.Debugln(err)
		} else {
			ret := DjangoStringResp(r.String())
			if len(ret) > 0 && ret != "ok" {
				if err := json.Unmarshal(r.Body(), &data); err != nil {
					a.Logger.Debugln(err)
				} else {
					if action, ok := data["recovery"].(string); ok {
						switch action {
						case "salt":
							go a.RecoverSalt()
						case "mesh":
							go a.RecoverMesh()
						case "command":
							if cmd, ok := data["cmd"].(string); ok {
								go a.RecoverCMD(cmd)
							}
						}
					}
				}
			}
		}
		sleep = randRange(30, 120)
		time.Sleep(time.Duration(sleep) * time.Second)
	}
}

func randRange(min, max int) int {
	rand.Seed(time.Now().UnixNano())
	return rand.Intn(max-min) + min
}
