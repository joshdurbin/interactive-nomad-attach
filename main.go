package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/signal"
	"sort"
	"time"

	"github.com/gdamore/tcell"

	"golang.org/x/sys/unix"

	log "github.com/Sirupsen/logrus"
	"github.com/docker/docker/pkg/term"
	"github.com/hashicorp/nomad/api"
	"github.com/rivo/tview"
	"github.com/spf13/viper"
)

const (
	welcomeModalPageName             = "welcome_modal"
	reattachModalPageName            = "reattach_modal"
	selectAllocationDropdownPageName = "select_allocation_dropdown"
	selectJobDropdownPageName        = "select_job_dropdown"
	selectTaskGroupPageName          = "select_task_group_dropdown"

	reconnectButton = "reconnect"
	quitButton      = "quit"

	yesButton = "yes"
	noButton  = "no"

	execCommand = "/bin/bash"

	welcomeModalText              = "Do you have an allocation UUID?"
	selectAllocationDropdownLabel = "Select an allocation: "
	selectJobDropdownLabel        = "Select a job: "
	selectTaskGroupDropdownLabel  = "Select a task group: "
	reattachModalText             = "An error occurred during attachment, code: %v, error: %v. Would you like to re-connect or quit?"

	errorPromptTimeout = 5 * time.Second
)

type app struct {
	*tview.Application
	viper       *viper.Viper
	nomadClient *api.Client
	logFile     *os.File
	pages       *tview.Pages

	// widgets need to be broken out individually so we can cross pull values while on other views or pages
	welcomeModal             *tview.Modal
	reattachModal            *tview.Modal
	selectAllocationDropdown *tview.DropDown
	selectJobDropdown        *tview.DropDown
	selectTaskGroupDropdown  *tview.DropDown

	lastUsedAllocationID string

	// timeout channel used when the error prompt is displayed
	timeoutChannel chan bool
}

func new() (*app, error) {

	// setup viper
	viper := viper.New()
	viper.SetConfigName("nomad-attach")
	viper.AddConfigPath("/etc")
	viper.AddConfigPath(".")
	viper.AutomaticEnv()
	err := viper.ReadInConfig()
	if err != nil {
		os.Exit(-1)
	}

	// setup, establish logging file
	logFile, err := os.OpenFile(viper.GetString("log.file"), os.O_APPEND|os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		log.Errorf("error opening file: %v", err)
		return nil, err
	}

	// setup logging
	log.SetFormatter(&log.JSONFormatter{})
	log.SetOutput(logFile)
	logLevel, err := log.ParseLevel(viper.GetString("log.level"))
	if err != nil {
		log.Errorf("log level cannot be parsed: %v", viper.GetString("log.level"))
		return nil, err
	}
	log.SetLevel(logLevel)

	// create nomad client
	var tlsConfig api.TLSConfig
	if viper.GetBool("nomad.use_tls") {
		tlsConfig = api.TLSConfig{
			CAPath:     viper.GetString("nomad.ca_path"),
			ClientCert: viper.GetString("nomad.client_cert"),
			ClientKey:  viper.GetString("nomad.client_key"),
		}
	}
	client, err := api.NewClient(&api.Config{
		Address:   viper.GetString("nomad.address"),
		TLSConfig: &tlsConfig,
	})
	if err != nil {
		log.Errorf("error instantiating nomad client, error: %v", err)
		return nil, err
	}

	app := &app{
		Application:              tview.NewApplication(),
		viper:                    viper,
		logFile:                  logFile,
		nomadClient:              client,
		pages:                    tview.NewPages(),
		welcomeModal:             tview.NewModal().SetText(welcomeModalText),
		selectAllocationDropdown: tview.NewDropDown().SetLabel(selectAllocationDropdownLabel),
		selectJobDropdown:        tview.NewDropDown().SetLabel(selectJobDropdownLabel),
		selectTaskGroupDropdown:  tview.NewDropDown().SetLabel(selectTaskGroupDropdownLabel),

		// don't set the text for the reattach modal as it's set via a queued updated in appContainerExec
		reattachModal: tview.NewModal(),
	}

	// add widgets to page object, only the first, the welcome modal, is initially set to visible
	app.pages.AddPage(welcomeModalPageName, app.welcomeModal, true, true).
		AddPage(selectAllocationDropdownPageName, app.selectAllocationDropdown, true, false).
		AddPage(selectJobDropdownPageName, app.selectJobDropdown, true, false).
		AddPage(selectTaskGroupPageName, app.selectTaskGroupDropdown, true, false).
		AddPage(reattachModalPageName, app.reattachModal, true, false)

	// define welcome modal
	app.welcomeModal.AddButtons([]string{yesButton, noButton}).
		SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			switch buttonLabel {
			case yesButton:
				app.pages.SwitchToPage(selectAllocationDropdownPageName)
			case noButton:
				app.pages.SwitchToPage(selectJobDropdownPageName)
			}
		})

	// define allocation selection, once an option is selected execute container attachment
	app.selectAllocationDropdown.
		SetOptions(app.getAllNomadAllocations(), func(text string, index int) {
			app.appContainerExec(text)
		}).SetDrawFunc(centerWidgetFunc).SetBorder(true)

	// define job selection, once an option is selected define task options on the task select widget
	// with complete functions that execute container attachment
	app.selectJobDropdown.
		SetOptions(app.getNomadJobs(), func(jobId string, index int) {

			app.getNomadTaskGroupsForJob(jobId)

			taskGroups := app.getNomadTaskGroupsForJob(jobId)

			if len(taskGroups) == 1 {
				taskGroup := taskGroups[0]
				app.appContainerExec(app.getRandomNomadAllocation(jobId, taskGroup))
			} else {
				app.selectTaskGroupDropdown.
					SetOptions(app.getNomadTaskGroupsForJob(jobId), func(taskGroup string, index int) {

						randomAllocation := app.getRandomNomadAllocation(jobId, taskGroup)
						app.appContainerExec(randomAllocation)
					})
				app.pages.SwitchToPage(selectTaskGroupPageName)
			}
		}).SetDrawFunc(centerWidgetFunc).SetBorder(true)

	// set draw func on task group selection
	app.selectTaskGroupDropdown.SetDrawFunc(centerWidgetFunc).SetBorder(true)

	// define reattach modal
	app.reattachModal.AddButtons([]string{reconnectButton, quitButton}).
		SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			switch buttonLabel {
			case reconnectButton:
				app.timeoutChannel <- true
				app.appContainerExec(app.lastUsedAllocationID)
			case quitButton:
				app.cleanupAndExit()
			}
		})

	return app, nil
}

func (a *app) errCleanupAndExit(errCode int) {
	a.Stop()
	os.Exit(errCode)
}

func (a *app) cleanupAndExit() {
	a.errCleanupAndExit(0)
}

func (a *app) getNomadJobs() []string {

	// get the initial list of jobs from
	jobs, _, err := a.nomadClient.Jobs().List(nil)

	if err != nil {
		log.Errorf("error retrieving jobs from nomad, error: %v", err)
		a.errCleanupAndExit(-1)
	}

	// pull out job names for selection
	values := make([]string, 0)
	for _, job := range jobs {

		if (a.viper.GetBool("query.jobs.service_required") && job.Type == api.JobTypeService) &&
			(a.viper.GetBool("query.jobs.must_be_running") && job.Status == api.AllocClientStatusRunning) {
			values = append(values, job.Name)
		}
	}
	return values
}

func (a *app) getNomadTaskGroupsForJob(jobID string) []string {

	values := make([]string, 0)
	job, _, err := a.nomadClient.Jobs().Info(jobID, nil)
	if err != nil {
		log.Errorf("error retrieving task groups from nomad, for job: %v error: %v", jobID, err)
		a.errCleanupAndExit(-1)
	}
	for _, taskGroup := range job.TaskGroups {
		values = append(values, *taskGroup.Name)
	}
	return values
}

func (a *app) getAllNomadAllocations() []string {

	return a.getNomadAllocations("", "")
}

func (a *app) getRandomNomadAllocation(jobID, taskGroup string) string {
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	runningAllocationIDs := a.getNomadAllocations(jobID, taskGroup)
	return runningAllocationIDs[r.Intn(len(runningAllocationIDs))]
}

func (a *app) getNomadAllocations(jobID, taskGroup string) []string {

	runningAllocationIDs := make([]string, 0)
	allocations, _, err := a.nomadClient.Allocations().List(nil)

	if err != nil {
		log.Errorf("error retrieving task groups from nomad, error: %v", err)
		a.errCleanupAndExit(-1)
	}
	for _, allocation := range allocations {
		if allocation.ClientStatus == api.AllocClientStatusRunning {
			if (jobID != "" && allocation.JobID == jobID || jobID == "") && (taskGroup != "" && allocation.TaskGroup == taskGroup || taskGroup == "") {
				runningAllocationIDs = append(runningAllocationIDs, allocation.ID)
			}
		}
	}

	sort.Strings(runningAllocationIDs)
	return runningAllocationIDs
}

// setRawTerminal sets the stream terminal in raw mode, so process captures
// Ctrl+C and other commands to forward to remote process.
// It returns a cleanup function that restores terminal to original mode.
func setRawTerminal(stream interface{}) (cleanup func(), err error) {
	fd, isTerminal := term.GetFdInfo(stream)
	if !isTerminal {
		return nil, errors.New("not a terminal")
	}

	state, err := term.SetRawTerminal(fd)
	if err != nil {
		return nil, err
	}

	return func() { term.RestoreTerminal(fd, state) }, nil
}

// setRawTerminalOutput sets the output stream in Windows to raw mode,
// so it disables LF -> CRLF translation.
// It's basically a no-op on unix.
func setRawTerminalOutput(stream interface{}) (cleanup func(), err error) {
	fd, isTerminal := term.GetFdInfo(stream)
	if !isTerminal {
		return nil, errors.New("not a terminal")
	}

	state, err := term.SetRawTerminalOutput(fd)
	if err != nil {
		return nil, err
	}

	return func() { term.RestoreTerminal(fd, state) }, nil
}

func setupWindowNotification(ch chan<- os.Signal) {
	signal.Notify(ch, unix.SIGWINCH)
}

// watchTerminalSize watches terminal size changes to propagate to remote tty.
func watchTerminalSize(out io.Writer, resize chan<- api.TerminalSize) (func(), error) {
	fd, isTerminal := term.GetFdInfo(out)
	if !isTerminal {
		return nil, errors.New("not a terminal")
	}

	ctx, cancel := context.WithCancel(context.Background())

	signalCh := make(chan os.Signal, 1)
	setupWindowNotification(signalCh)

	sendTerminalSize := func() {
		s, err := term.GetWinsize(fd)
		if err != nil {
			return
		}

		resize <- api.TerminalSize{
			Height: int(s.Height),
			Width:  int(s.Width),
		}
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-signalCh:
				sendTerminalSize()
			}
		}
	}()

	go func() {
		// send initial size
		sendTerminalSize()
	}()

	return cancel, nil
}

func (a *app) appContainerExec(allocationUUID string) {

	a.Application.Suspend(func() {

		errorCode, err := a.containerExec(allocationUUID)

		if err != nil {

			a.lastUsedAllocationID = allocationUUID

			a.QueueUpdate(func() {
				a.reattachModal.SetText(fmt.Sprintf(reattachModalText, errorCode, err))
				a.pages.SwitchToPage(reattachModalPageName)

				// use a non-buffered channel so we don't block if / when a user selects re-connect
				a.timeoutChannel = make(chan bool, 1)

				go func() {
					select {
					case <-a.timeoutChannel:
						// do nothing
					case <-time.After(errorPromptTimeout):
						a.cleanupAndExit()
					}
				}()
			})
		} else {
			a.cleanupAndExit()
		}
	})
}

func (a *app) containerExec(allocationUUID string) (int, error) {

	// get the actual allocation from nomad
	alloc, _, err := a.nomadClient.Allocations().Info(allocationUUID, nil)

	if err != nil {
		log.Errorf("unable to find allocation data for uuid %v", allocationUUID)
		return -1, err
	}

	sizeCh := make(chan api.TerminalSize, 1)

	ctx, cancelFn := context.WithCancel(context.Background())
	defer cancelFn()

	var exitCode int

	inCleanup, err := setRawTerminal(os.Stdin)
	if err != nil {
		return -1, err
	}
	defer inCleanup()

	outCleanup, err := setRawTerminalOutput(os.Stdout)
	if err != nil {
		return -1, err
	}
	defer outCleanup()

	sizeCleanup, err := watchTerminalSize(os.Stdout, sizeCh)
	if err != nil {
		return -1, err
	}
	defer sizeCleanup()

	exitCode, err = a.nomadClient.Allocations().Exec(ctx, alloc, alloc.TaskGroup, true, []string{execCommand}, os.Stdin, os.Stdout,
		os.Stderr, sizeCh, nil)

	if err != nil {
		return -1, err
	}

	return exitCode, err
}

func centerWidgetFunc(screen tcell.Screen, x int, y int, width int, height int) (int, int, int, int) {
	centerY := y + height/2
	centerX := x + width/3
	return centerX + 1, centerY - 1, width - 2, height - (centerY + 1 - y)
}

func main() {

	app, err := new()
	if err != nil {
		log.Error(err)
		os.Exit(-1)
	}
	defer app.logFile.Close()

	err = app.SetRoot(app.pages, true).Run()
	if err != nil {
		log.Error(err)
		os.Exit(-1)
	}

}
