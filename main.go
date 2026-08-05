//go:build windows

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/windows/svc"
)

const serviceName = "ApiLauncherService"

type Config struct {
	Nginx string      `json:"nginx"`
	APP   []APIConfig `json:"app"`
}

type APIConfig struct {
	Path string `json:"path"`
}

type LauncherService struct {
	processes []*exec.Cmd
	mutex     sync.Mutex
	logger    *log.Logger
}

func main() {
	baseDir, err := getBaseDir()
	if err != nil {
		return
	}

	logFile, err := os.OpenFile(
		filepath.Join(baseDir, "service.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY,
		0666,
	)
	if err != nil {
		return
	}
	defer logFile.Close()

	logger := log.New(logFile, "", log.Ldate|log.Ltime)

	isService, err := svc.IsWindowsService()
	if err != nil {
		logger.Printf("Error verificando servicio: %v", err)
		return
	}

	if !isService {
		logger.Println("Este ejecutable debe iniciarse como servicio de Windows")
		return
	}

	service := &LauncherService{
		logger: logger,
	}

	if err := svc.Run(serviceName, service); err != nil {
		logger.Printf("Error ejecutando el servicio: %v", err)
	}
}

func (s *LauncherService) Execute(
	args []string,
	requests <-chan svc.ChangeRequest,
	status chan<- svc.Status,
) (bool, uint32) {
	status <- svc.Status{
		State: svc.StartPending,
	}

	if err := s.startProcesses(); err != nil {
		s.logger.Printf("Error iniciando procesos: %v", err)

		status <- svc.Status{
			State: svc.StopPending,
		}

		return true, 1
	}

	status <- svc.Status{
		State:   svc.Running,
		Accepts: svc.AcceptStop | svc.AcceptShutdown,
	}

	s.logger.Println("Servicio iniciado correctamente")

	for request := range requests {
		switch request.Cmd {
		case svc.Interrogate:
			status <- request.CurrentStatus

		case svc.Stop, svc.Shutdown:
			status <- svc.Status{
				State: svc.StopPending,
			}

			s.logger.Println("Deteniendo procesos")
			s.stopProcesses()

			return false, 0
		}
	}

	return false, 0
}

func (s *LauncherService) startProcesses() error {
	baseDir, err := getBaseDir()
	if err != nil {
		return err
	}

	configPath := filepath.Join(baseDir, "config.json")

	config, err := loadConfig(configPath)
	if err != nil {
		return err
	}

	if strings.TrimSpace(config.Nginx) != "" {
		if err := s.startProcess(config.Nginx, baseDir); err != nil {
			return fmt.Errorf("no se pudo iniciar nginx: %w", err)
		}
	}

	for _, app := range config.APP {
		if strings.TrimSpace(app.Path) == "" {
			continue
		}

		if err := s.startProcess(app.Path, baseDir); err != nil {
			s.logger.Printf(
				"No se pudo iniciar %s: %v",
				app.Path,
				err,
			)
		}
	}

	return nil
}

func (s *LauncherService) startProcess(
	configuredPath string,
	baseDir string,
) error {
	executablePath := configuredPath

	if !filepath.IsAbs(executablePath) {
		executablePath = filepath.Join(baseDir, executablePath)
	}

	executablePath, err := filepath.Abs(executablePath)
	if err != nil {
		return err
	}

	if _, err := os.Stat(executablePath); err != nil {
		return fmt.Errorf(
			"no existe el ejecutable %s: %w",
			executablePath,
			err,
		)
	}

	cmd := exec.Command(executablePath)


	cmd.Dir = filepath.Dir(executablePath)

	cmd.Stdout = s.logger.Writer()
	cmd.Stderr = s.logger.Writer()

	if err := cmd.Start(); err != nil {
		return err
	}

	s.mutex.Lock()
	s.processes = append(s.processes, cmd)
	s.mutex.Unlock()

	s.logger.Printf(
		"Proceso iniciado: %s, PID: %d",
		executablePath,
		cmd.Process.Pid,
	)

	go func() {
		err := cmd.Wait()

		if err != nil {
			s.logger.Printf(
				"Proceso terminado con error: %s: %v",
				executablePath,
				err,
			)
		} else {
			s.logger.Printf(
				"Proceso terminado: %s",
				executablePath,
			)
		}
	}()

	return nil
}

func (s *LauncherService) stopProcesses() {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	for index := len(s.processes) - 1; index >= 0; index-- {
		cmd := s.processes[index]

		if cmd == nil || cmd.Process == nil {
			continue
		}

		pid := cmd.Process.Pid

		taskkill := exec.Command(
			"taskkill.exe",
			"/PID",
			fmt.Sprintf("%d", pid),
			"/T",
			"/F",
		)

		if output, err := taskkill.CombinedOutput(); err != nil {
			s.logger.Printf(
				"No se pudo detener PID %d: %v, salida: %s",
				pid,
				err,
				string(output),
			)
		} else {
			s.logger.Printf("Proceso detenido, PID: %d", pid)
		}
	}

	s.processes = nil
}

func loadConfig(path string) (*Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf(
			"no se pudo abrir config.json: %w",
			err,
		)
	}
	defer file.Close()

	var config Config

	if err := json.NewDecoder(file).Decode(&config); err != nil {
		return nil, fmt.Errorf(
			"config.json inválido: %w",
			err,
		)
	}

	return &config, nil
}

func getBaseDir() (string, error) {
	executablePath, err := os.Executable()
	if err != nil {
		return "", err
	}

	return filepath.Dir(executablePath), nil
}
