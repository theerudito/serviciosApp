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

	logger := log.New(
		logFile,
		"",
		log.Ldate|log.Ltime|log.Lmicroseconds,
	)

	logger.Printf("Directorio del servicio: %s", baseDir)

	isService, err := svc.IsWindowsService()
	if err != nil {
		logger.Printf("Error verificando servicio: %v", err)
		return
	}

	if !isService {
		logger.Println(
			"Este ejecutable debe iniciarse como servicio de Windows",
		)
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

		s.stopProcesses()

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

		default:
			s.logger.Printf(
				"Solicitud de servicio no soportada: %d",
				request.Cmd,
			)
		}
	}

	return false, 0
}

func (s *LauncherService) startProcesses() error {
	baseDir, err := getBaseDir()
	if err != nil {
		return fmt.Errorf(
			"no se pudo obtener el directorio del servicio: %w",
			err,
		)
	}


	appDir := filepath.Join(baseDir, "app")

	if err := ensureAppDirectory(appDir); err != nil {
		return err
	}

	s.logger.Printf("Carpeta app verificada: %s", appDir)

	configPath := filepath.Join(baseDir, "config.json")

	config, err := loadConfig(configPath)
	if err != nil {
		return err
	}


	nginxPath := strings.TrimSpace(config.Nginx)

	if nginxPath != "" {
		if err := s.startProcess(nginxPath, baseDir); err != nil {
			return fmt.Errorf(
				"no se pudo iniciar nginx: %w",
				err,
			)
		}
	} else {
		s.logger.Println(
			"Nginx no está configurado; se omite su inicio",
		)
	}


	startedApps := 0

	for index, app := range config.APP {
		appPath := strings.TrimSpace(app.Path)

		if appPath == "" {
			s.logger.Printf(
				"Aplicación en posición %d ignorada: ruta vacía",
				index,
			)
			continue
		}

		if err := s.startProcess(appPath, appDir); err != nil {
			s.logger.Printf(
				"No se pudo iniciar la aplicación %q: %v",
				appPath,
				err,
			)


			continue
		}

		startedApps++
	}

	s.logger.Printf(
		"Aplicaciones configuradas: %d; iniciadas correctamente: %d",
		len(config.APP),
		startedApps,
	)

	return nil
}

func ensureAppDirectory(appDir string) error {
	info, err := os.Stat(appDir)

	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf(
				"la ruta app existe, pero no es una carpeta: %s",
				appDir,
			)
		}

		return nil
	}

	if !os.IsNotExist(err) {
		return fmt.Errorf(
			"no se pudo verificar la carpeta app: %w",
			err,
		)
	}

	if err := os.MkdirAll(appDir, 0755); err != nil {
		return fmt.Errorf(
			"no se pudo crear la carpeta app %s: %w",
			appDir,
			err,
		)
	}

	return nil
}

func (s *LauncherService) startProcess(
	configuredPath string,
	referenceDir string,
) error {
	configuredPath = strings.TrimSpace(configuredPath)

	if configuredPath == "" {
		return fmt.Errorf("la ruta del ejecutable está vacía")
	}

	executablePath := filepath.Clean(configuredPath)

	
	if !filepath.IsAbs(executablePath) {
		executablePath = filepath.Join(
			referenceDir,
			executablePath,
		)
	}

	executablePath, err := filepath.Abs(executablePath)
	if err != nil {
		return fmt.Errorf(
			"no se pudo resolver la ruta %q: %w",
			configuredPath,
			err,
		)
	}

	info, err := os.Stat(executablePath)
	if err != nil {
		return fmt.Errorf(
			"no existe el ejecutable %s: %w",
			executablePath,
			err,
		)
	}

	if info.IsDir() {
		return fmt.Errorf(
			"la ruta corresponde a una carpeta, no a un ejecutable: %s",
			executablePath,
		)
	}

	if !strings.EqualFold(
		filepath.Ext(executablePath),
		".exe",
	) {
		return fmt.Errorf(
			"el archivo no tiene extensión .exe: %s",
			executablePath,
		)
	}

	cmd := exec.Command(executablePath)


	cmd.Dir = filepath.Dir(executablePath)

	cmd.Stdout = s.logger.Writer()
	cmd.Stderr = s.logger.Writer()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf(
			"no se pudo ejecutar %s: %w",
			executablePath,
			err,
		)
	}

	s.mutex.Lock()
	s.processes = append(s.processes, cmd)
	s.mutex.Unlock()

	s.logger.Printf(
		"Proceso iniciado: %s, PID: %d",
		executablePath,
		cmd.Process.Pid,
	)

	go s.waitForProcess(cmd, executablePath)

	return nil
}

func (s *LauncherService) waitForProcess(
	cmd *exec.Cmd,
	executablePath string,
) {
	err := cmd.Wait()

	if err != nil {
		s.logger.Printf(
			"Proceso terminado con error: %s: %v",
			executablePath,
			err,
		)
	} else {
		s.logger.Printf(
			"Proceso terminado correctamente: %s",
			executablePath,
		)
	}

	s.removeProcess(cmd)
}

func (s *LauncherService) removeProcess(target *exec.Cmd) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	for index, cmd := range s.processes {
		if cmd != target {
			continue
		}

		s.processes = append(
			s.processes[:index],
			s.processes[index+1:]...,
		)

		return
	}
}

func (s *LauncherService) stopProcesses() {

	s.mutex.Lock()

	processes := make([]*exec.Cmd, len(s.processes))
	copy(processes, s.processes)

	s.processes = nil

	s.mutex.Unlock()

	for index := len(processes) - 1; index >= 0; index-- {
		cmd := processes[index]

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

		output, err := taskkill.CombinedOutput()
		if err != nil {
			s.logger.Printf(
				"No se pudo detener PID %d: %v, salida: %s",
				pid,
				err,
				strings.TrimSpace(string(output)),
			)
			continue
		}

		s.logger.Printf(
			"Proceso detenido, PID: %d, salida: %s",
			pid,
			strings.TrimSpace(string(output)),
		)
	}
}

func loadConfig(path string) (*Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf(
			"no se pudo abrir config.json en %s: %w",
			path,
			err,
		)
	}
	defer file.Close()

	var config Config

	decoder := json.NewDecoder(file)

	if err := decoder.Decode(&config); err != nil {
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
		return "", fmt.Errorf(
			"no se pudo obtener la ruta del ejecutable: %w",
			err,
		)
	}

	executablePath, err = filepath.Abs(executablePath)
	if err != nil {
		return "", fmt.Errorf(
			"no se pudo resolver la ruta del ejecutable: %w",
			err,
		)
	}

	return filepath.Dir(executablePath), nil
}
