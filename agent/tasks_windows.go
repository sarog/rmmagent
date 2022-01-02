package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/capnspacehook/taskmaster"
	rmm "github.com/sarog/rmmagent/shared"
)

func (a *Agent) RunTask(id int) error {
	data := rmm.AutomatedTask{}
	url := fmt.Sprintf("/api/v3/%d/%s/taskrunner/", id, a.AgentID)
	r1, gerr := a.rClient.R().Get(url)
	if gerr != nil {
		a.Logger.Debugln(gerr)
		return gerr
	}

	if r1.IsError() {
		a.Logger.Debugln("Run Task:", r1.String())
		return nil
	}

	if err := json.Unmarshal(r1.Body(), &data); err != nil {
		a.Logger.Debugln(err)
		return err
	}

	start := time.Now()
	stdout, stderr, retcode, _ := a.RunScript(data.TaskScript.Code, data.TaskScript.Shell, data.Args, data.Timeout)

	type TaskResult struct {
		Stdout   string  `json:"stdout"`
		Stderr   string  `json:"stderr"`
		RetCode  int     `json:"retcode"`
		ExecTime float64 `json:"execution_time"`
	}

	payload := TaskResult{Stdout: stdout, Stderr: stderr, RetCode: retcode, ExecTime: time.Since(start).Seconds()}

	_, perr := a.rClient.R().SetBody(payload).Patch(url)
	if perr != nil {
		a.Logger.Debugln(perr)
		return perr
	}
	return nil
}

// CreateInternalTask creates predefined tacticalrmm internal tasks
func (a *Agent) CreateInternalTask(name, args, repeat string, start int) (bool, error) {
	conn, err := taskmaster.Connect()
	if err != nil {
		return false, err
	}
	defer conn.Disconnect()

	def := conn.NewTaskDefinition()

	dailyTrigger := taskmaster.DailyTrigger{
		TaskTrigger: taskmaster.TaskTrigger{
			Enabled:       true,
			StartBoundary: time.Now().Add(time.Duration(start) * time.Minute),
		},
		DayInterval: taskmaster.EveryDay,
	}

	def.AddTrigger(dailyTrigger)

	action := taskmaster.ExecAction{
		Path:       "tacticalrmm.exe",
		WorkingDir: a.ProgramDir,
		Args:       args,
	}
	def.AddAction(action)

	def.Principal.RunLevel = taskmaster.TASK_RUNLEVEL_HIGHEST
	def.Principal.LogonType = taskmaster.TASK_LOGON_SERVICE_ACCOUNT
	def.Principal.UserID = "SYSTEM"
	def.Settings.AllowDemandStart = true
	def.Settings.AllowHardTerminate = true
	def.Settings.DontStartOnBatteries = false
	def.Settings.Enabled = true
	def.Settings.MultipleInstances = taskmaster.TASK_INSTANCES_PARALLEL
	def.Settings.StopIfGoingOnBatteries = false
	def.Settings.WakeToRun = true

	_, success, err := conn.CreateTask(fmt.Sprintf("\\%s", name), def, true)
	if err != nil {
		return false, err
	}

	if success {
		// https://github.com/capnspacehook/taskmaster/issues/15
		out, err := CMD("schtasks", []string{"/Change", "/TN", name, "/RI", repeat}, 10, false)
		if err != nil {
			return false, err
		}
		if out[1] != "" {
			a.Logger.Errorln(out[1])
			return false, nil
		}
		return success, nil
	}
	return false, nil
}

// SchedTask Scheduled Task
// 2021-12-31: used in:
// 		api/tacticalrmm/agents/views.py:389
type SchedTask struct {
	PK                 int                  `json:"pk"`
	Type               string               `json:"type"` // rmm, custom
	Name               string               `json:"name"`
	Trigger            string               `json:"trigger"` // re: "task_type": manual, checkfailure, runonce, daily, weekly, monthly, monthlydow
	Enabled            bool                 `json:"enabled"`
	DeleteAfter        bool                 `json:"deleteafter"`
	WeekDays           taskmaster.DayOfWeek `json:"weekdays"`
	Year               int                  `json:"year"`
	Month              string               `json:"month"`
	Day                int                  `json:"day"`
	Hour               int                  `json:"hour"`
	Minute             int                  `json:"min"`
	Path               string               `json:"path"`
	WorkDir            string               `json:"workdir"`
	Args               string               `json:"args"`
	Parallel           bool                 `json:"parallel"`
	RunASAPAfterMissed bool                 `json:"run_asap_after_missed"`
	// todo: 1.7.3+: OverwriteTask bool `json:"overwrite_task"` // 2022-01-01: via nats: api/tacticalrmm/autotasks/models.py:357
	// todo: 1.7.3+: MultipleInstances int `json:"multiple_instances"`
	// todo: 1.7.3+: DeletedExpiredAfter bool `json:"delete_expired_task_after"`
	// todo: 1.7.3+: StartWhenAvailable bool `json:"start_when_available"`
	// run_on_last_day_of_month, random_delay, repetition_interval, repetition_duration,
	// days_of_week, days_of_month, weeks_of_month, months_of_year

}

func (a *Agent) CreateSchedTask(st SchedTask) (bool, error) {
	conn, err := taskmaster.Connect()
	if err != nil {
		a.Logger.Errorln(err)
		return false, err
	}
	defer conn.Disconnect()

	var trigger taskmaster.Trigger
	var action taskmaster.ExecAction
	var path, workdir, args string
	def := conn.NewTaskDefinition()

	now := time.Now()
	switch st.Trigger {
	case "once":
		if st.DeleteAfter {
			deleteAfterDate := time.Date(st.Year, getMonth(st.Month), st.Day, st.Hour, st.Minute, 0, 0, now.Location())
			trigger = taskmaster.TimeTrigger{
				TaskTrigger: taskmaster.TaskTrigger{
					Enabled:       true,
					StartBoundary: deleteAfterDate,
					EndBoundary:   deleteAfterDate.Add(10 * time.Minute),
				},
			}
		} else {
			trigger = taskmaster.TimeTrigger{
				TaskTrigger: taskmaster.TaskTrigger{
					Enabled:       true,
					StartBoundary: time.Date(st.Year, getMonth(st.Month), st.Day, st.Hour, st.Minute, 0, 0, now.Location()),
				},
			}
		}
	case "weekly":
		trigger = taskmaster.WeeklyTrigger{
			TaskTrigger: taskmaster.TaskTrigger{
				Enabled:       true,
				StartBoundary: time.Date(now.Year(), now.Month(), now.Day(), st.Hour, st.Minute, 0, 0, now.Location()),
			},
			DaysOfWeek:   st.WeekDays,
			WeekInterval: taskmaster.EveryWeek,
		}
	case "manual":
		trigger = taskmaster.TimeTrigger{
			TaskTrigger: taskmaster.TaskTrigger{
				Enabled:       true,
				StartBoundary: time.Date(1975, 1, 1, 1, 0, 0, 0, now.Location()),
			},
		}
		// todo: from 1.7.3+:
		//  case "checkfailure":
	}

	def.AddTrigger(trigger)

	switch st.Type {
	case "rmm":
		path = "tacticalrmm.exe"
		workdir = a.ProgramDir
		args = fmt.Sprintf("-m taskrunner -p %d", st.PK)
	case "schedreboot":
		// 2022-01-01: via nats_cmd: api/tacticalrmm/agents/views.py:390
		path = "shutdown.exe"
		workdir = filepath.Join(os.Getenv("SYSTEMROOT"), "System32")
		args = "/r /t 5 /f"
	case "custom":
		path = st.Path
		workdir = st.WorkDir
		args = st.Args
	}

	action = taskmaster.ExecAction{
		Path:       path,
		WorkingDir: workdir,
		Args:       args,
	}
	def.AddAction(action)

	def.Principal.RunLevel = taskmaster.TASK_RUNLEVEL_HIGHEST
	def.Principal.LogonType = taskmaster.TASK_LOGON_SERVICE_ACCOUNT
	def.Principal.UserID = "SYSTEM"
	def.Settings.AllowDemandStart = true
	def.Settings.AllowHardTerminate = true
	def.Settings.DontStartOnBatteries = false
	def.Settings.Enabled = true
	def.Settings.StopIfGoingOnBatteries = false
	def.Settings.WakeToRun = true
	if st.DeleteAfter {
		def.Settings.DeleteExpiredTaskAfter = "PT15M"
	}
	if st.Parallel {
		def.Settings.MultipleInstances = taskmaster.TASK_INSTANCES_PARALLEL
	} else {
		def.Settings.MultipleInstances = taskmaster.TASK_INSTANCES_IGNORE_NEW
	}

	if st.RunASAPAfterMissed {
		def.Settings.StartWhenAvailable = true
	}

	_, success, err := conn.CreateTask(fmt.Sprintf("\\%s", st.Name), def, true)
	if err != nil {
		a.Logger.Errorln(err)
		return false, err
	}

	return success, nil
}

// DeleteSchedTask Deletes a Scheduled Task
func DeleteSchedTask(name string) error {
	conn, err := taskmaster.Connect()
	if err != nil {
		return err
	}
	defer conn.Disconnect()

	err = conn.DeleteTask(fmt.Sprintf("\\%s", name))
	if err != nil {
		return err
	}
	return nil
}

// EnableSchedTask Enables a Scheduled Task
func EnableSchedTask(st SchedTask) error {
	conn, err := taskmaster.Connect()
	if err != nil {
		return err
	}
	defer conn.Disconnect()

	task, err := conn.GetRegisteredTask(fmt.Sprintf("\\%s", st.Name))
	if err != nil {
		return err
	}

	task.Definition.Settings.Enabled = st.Enabled

	_, err = conn.UpdateTask(task.Path, task.Definition)
	if err != nil {
		return err
	}
	return nil
}

// CleanupSchedTasks removes all tacticalrmm scheduled tasks during uninstall
func CleanupSchedTasks() {
	conn, err := taskmaster.Connect()
	if err != nil {
		return
	}
	defer conn.Disconnect()

	tasks, err := conn.GetRegisteredTasks()
	if err != nil {
		return
	}

	for _, task := range tasks {
		if strings.HasPrefix(task.Name, "TacticalRMM_") {
			conn.DeleteTask(fmt.Sprintf("\\%s", task.Name))
		}
	}
	tasks.Release()
}

func ListSchedTasks() []string {
	ret := make([]string, 0)

	conn, err := taskmaster.Connect()
	if err != nil {
		return ret
	}
	defer conn.Disconnect()

	tasks, err := conn.GetRegisteredTasks()
	if err != nil {
		return ret
	}

	for _, task := range tasks {
		ret = append(ret, task.Name)
	}
	tasks.Release()
	return ret
}

func getMonth(month string) time.Month {
	switch month {
	case "January":
		return time.January
	case "February":
		return time.February
	case "March":
		return time.March
	case "April":
		return time.April
	case "May":
		return time.May
	case "June":
		return time.June
	case "July":
		return time.July
	case "August":
		return time.August
	case "September":
		return time.September
	case "October":
		return time.October
	case "November":
		return time.November
	case "December":
		return time.December
	default:
		return time.January
	}
}
