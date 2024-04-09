//=============================================================================
// Code for managing HPC jobs deployed on SLURM
//=============================================================================

package main

import (
  "encoding/json"
  "golang.org/x/crypto/ssh"
	"fmt"
	"os"
  "bytes"
	"strconv"
	"strings"
	"time"

	"github.com/ibm/bridge-operator/podutils"

	"k8s.io/klog"
)

const (
	MULTIPLE_ACCEPT_TYPE = "text/plain,application/xml,text/xml,multipart/mixed"
	TIME                 = "2006-01-02T15:04:05Z"

	SUBMITTED  = "SUBMITTED"
	PENDING    = "PENDING"
	RUNNING    = "RUNNING"
	COMPLETED  = "COMPLETED"
	COMPLETING = "COMPLETING"
	CANCELLED  = "CANCELLED"
	UNKNOWN    = "UNKNOWN"
	FAILED     = "FAILED"

	CREDS_DIR  = "/credentials/"
	SCRIPT_DIR = "/script/script"
	FILES_DIR  = "/downloads/"

	TOKEN_SLEEP = 3
)

var HPCURL string
var JOB_NAME string
var NAMESPACE string
var POLL int
var S3 string
var UPLOAD string
var DOWNLOAD string
var JobProp map[string]string

type (
	JobInfo struct {
		Job []JobMap `json:"jobs"`
	}

	JobMap struct {
		States     []string `json:"job_state"`
		StartTime  int   `json:"start_time"`
		SubmitTime int   `json:"submit_time"`
		EndTime    int   `json:"end_time"`
	}
)

// HPC job resource definitions
var RESOURCES = map[string]string{
	"RunLimitHour":   "RUNLIMITHOUR",
	"RunLimitMinute": "RUNLIMITMINUTE",
	"Queue":          "QUEUE",
	"OutputFileName": "OUTPUT_FILE",
	"ErrorFileName":  "ERROR_FILE",
}

// Gets detailed job information for jobs that have the specified job IDs.
func getJobInfo(slurmUsername string, slurmPassword string, id string) *JobMap {
    sshConfig := &ssh.ClientConfig{
        User: slurmUsername,
        Auth: []ssh.AuthMethod{
            ssh.Password(slurmPassword),
        },
        HostKeyCallback: ssh.InsecureIgnoreHostKey(),
    }

    client, err := ssh.Dial("tcp", HPCURL + ":22", sshConfig)
    if err != nil {
        klog.Error("Failed to dial: %s", err)
    }

    session, err := client.NewSession()
    if err != nil {
        klog.Error("Failed to create session: %s", err)
    }
    defer session.Close()

    var b bytes.Buffer
    session.Stdout = &b
    if err := session.Run("scontrol show job --json " + id); err != nil {
        klog.Error("Failed to run: %s", err)
    }

    job := JobInfo{}
    err = json.Unmarshal(b.Bytes(), &job)
    jobMap := job.Job[0]
    return &jobMap
}


func checkSlurmAccess(slurmUsername string, slurmPassword string) {
    sshConfig := &ssh.ClientConfig{
        User: slurmUsername,
        Auth: []ssh.AuthMethod{
            ssh.Password(slurmPassword),
        },
        HostKeyCallback: ssh.InsecureIgnoreHostKey(),
    }

    _, err := ssh.Dial("tcp", HPCURL + ":22", sshConfig)
    if err != nil {
        klog.Exit("Ping to HPC cluster not successful, check SLURM Token ", err)
    }
}


// Build body for the HPC job submission
func buildBody(script string) string {
	var xmlString string
	xmlPart := fmt.Sprintf("{\"job\":{\"partition\":\"%s\",\"tasks\":%s,\"name\":\"%s\",\"nodes\":%s,\"current_working_directory\":\"%s\",\"environment\":{\"PATH\":\"%s\",\"LD_LIBRARY_PATH\":\"%s\"}},\"script\":\"", JobProp["Queue"], JobProp["Tasks"], JobProp["slurmJobName"], JobProp["NodesNumber"], JobProp["currentWorkingDir"], JobProp["envPath"], JobProp["envLibPath"])
	xmlString += fmt.Sprintf("%s%s%s\"} ", xmlString, xmlPart, script)
	return xmlString
}

// Submit request for job execution
func submit(slurmUsername string, slurmPassword string, data map[string]string) int {
    jobscript := data["jobdata.jobScript"]

    sshConfig := &ssh.ClientConfig{
        User: slurmUsername,
        Auth: []ssh.AuthMethod{
            ssh.Password(slurmPassword),
        },
        HostKeyCallback: ssh.InsecureIgnoreHostKey(),
    }

    client, err := ssh.Dial("tcp", HPCURL + ":22", sshConfig)
    if err != nil {
        klog.Error("Failed to dial: %s", err)
        return 0
    }

    session, err := client.NewSession()
    if err != nil {
        klog.Error("Failed to create session: %s", err)
        return 0
    }
    defer session.Close()

    var b bytes.Buffer
    var berr bytes.Buffer
    session.Stdout = &b
    session.Stderr = &berr

    if err := session.Run("echo -e '" + jobscript + "' | sbatch"); err != nil {
        klog.Error("Failed to run: %s\n%s", err, berr.String())
        return 0
    }

    resp := b.String()
    parts := strings.Split(resp, " ")
    if len(parts) < 2 {
        klog.Error("Job submission failed: unexpected sbatch message: %s", b.String())
        return 0
    }

    id, err := strconv.Atoi(strings.TrimSuffix(parts[3], "\n"))
    if err != nil {
      klog.Error("Job submission failed: %s", err)
        return 0
    }

    klog.Info("Successfully submitted a job with job id ", id)
    return id
}

// Kill HPC Job
func kill(slurmUsername string, slurmPassword string, id string) string {
    sshConfig := &ssh.ClientConfig{
        User: slurmUsername,
        Auth: []ssh.AuthMethod{
            ssh.Password(slurmPassword),
        },
        HostKeyCallback: ssh.InsecureIgnoreHostKey(),
    }

    client, err := ssh.Dial("tcp", HPCURL + ":22", sshConfig)
    if err != nil {
        return fmt.Sprintf("Failed to create SSH connection to kill job, err: %s", err)
    }

    session, err := client.NewSession()
    if err != nil {
        return fmt.Sprintf("Failed to create SSH session to kill job, err: %s", err)
    }
    defer session.Close()

    var b bytes.Buffer
    session.Stdout = &b
    if err := session.Run("scancel " + id); err != nil {
        return fmt.Sprintf("Failed to execute job kill, err %s", err)

    }

    return b.String()
}

// Get login token
func getUsernamePassword() (string, string) {
	username := podutils.ReadMountedFileContent(CREDS_DIR + "username")
	password := podutils.ReadMountedFileContent(CREDS_DIR + "password")
	return username, password
}

// Add additional information from HPC
func getAdditionalInfo(jobMap *JobMap, info map[string]string) {
	start := jobMap.StartTime
	if start != 0 {
		info["startTime"] = fmt.Sprint(start)
	}
	sub := jobMap.SubmitTime
	if sub != 0 {
		info["submitTime"] = fmt.Sprint(sub)
	}
	end := jobMap.EndTime
	if end != 0 {
		info["endTime"] = fmt.Sprint(end)
	}
}

// Kill the job
func killJob(slurmUsername string, slurmPassword string, id string, state string, info map[string]string) {
	// Check if the job is still running
	running := state != CANCELLED && state != COMPLETED && state != FAILED
	if running {
		// Only kill jobs that are still running
		res := kill(slurmUsername, slurmPassword, id)
		if len(res) == 0 {
			klog.Info("Job", id, "killed successfully.")
			info["jobStatus"] = CANCELLED
		} else {
			klog.Info("Job ", id, " is not killed; msg: ", res, ". Continue in monitoring, will try to kill again.")
		}
	} else {
		info["jobStatus"] = CANCELLED
		klog.Info("Job ", id, " is already in finished state ", state)
	}
}

// Monitoring job execution
// Method that runs constantly monitoring HPC job
func monitor(slurmUsername string, slurmPassword string, info map[string]string) {
	id := info["id"]
	// Run forever
	for {
		// Sleep before next run
		time.Sleep(time.Duration(POLL) * time.Second)

		// Get current config map
		cm := podutils.GetConfigMap()

		// Get current execution status and update config map
		var state = ""
		jobMap := getJobInfo(slurmUsername, slurmPassword, id)
		if jobMap != nil {
			jstate := jobMap.States[0]
			state = fmt.Sprint(jstate)
			info["jobStatus"] = state
			if state == COMPLETED || state == COMPLETING || state == CANCELLED || state == FAILED {
				// Get additional info from HPC job
				getAdditionalInfo(jobMap, info)
			} else {
				// Check for kill flag
				if cm.Data["kill"] == "true" {
					killJob(slurmUsername, slurmPassword, id, info["jobStatus"], info)
				}
			}
			podutils.UpdateConfigMap(cm, info)
		}
		// Terminate if we are done
		if state == COMPLETED {
			os.Exit(0)
		}
		if state == CANCELLED || state == FAILED {
			os.Exit(1)
		}

	}
}

// Main method
func main() {

  klog.Info("In main !!!")
	// Get namespace and job name from environment
	NAMESPACE = os.Getenv("NAMESPACE")
	JOB_NAME = os.Getenv("JOBNAME")

	// Initialize utils
	podutils.InitUtils(JOB_NAME, NAMESPACE)

	// Get config map and its parameters
	cm := podutils.GetConfigMap()
	HPCURL = cm.Data["resourceURL"]
	POLL, _ = strconv.Atoi(cm.Data["updateInterval"])
	S3 = cm.Data["s3.secret"]

	// Get Access Username, Token for Slurm  cluster
	slurmUsername, slurmPassword := getUsernamePassword()
	checkSlurmAccess(slurmUsername, slurmPassword)

	if len(slurmPassword) == 0 || len(slurmUsername) == 0 {
		// Failed to get credentials for HPC cluster
		klog.Exit("Failed to get access token for HPC cluster")
	}

	// Get ID from config map
	id := cm.Data["id"]

	// create info for keeping track of execution parameters
	info := make(map[string]string)
	info["startTime"] = ""
	info["endTime"] = ""
	info["message"] = ""

	// If an ID is present in the config map it means that that we have already started a job
	if len(id) == 0 {
		klog.Info("Slurm Job with name ", JOB_NAME, " does not exist. Submitting new job.")

		intId := submit(slurmUsername, slurmPassword, cm.Data)
		id = fmt.Sprint(intId)

		if len(id) == 0 {
			// Failed to submit a job
			info["jobStatus"] = FAILED
			info["message"] = "Failed to submit a job to HPC"
		} else {
			info["id"] = id
			info["jobStatus"] = SUBMITTED
			info["startTime"] = time.Now().Format(TIME)
		}

		podutils.UpdateConfigMap(cm, info)

		// Start monitoring or exit
		if len(id) != 0 {
			monitor(slurmUsername, slurmPassword, info)
		} else {
			klog.Exit("Failed to start HPC job")
		}
	} else {
		// Job is already running
		klog.Info("Slurm Job  has associated ID in ConfigMap. Handling state.")
		info["id"] = id
		monitor(slurmUsername, slurmPassword, info)
	}
}
