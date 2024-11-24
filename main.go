package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

// Session represents the session structure
type Session struct {
	hash       string
	bridgeName string
	tapNames   map[string]string // Key - Machine ID, Value - TAP name
	ptyFiles   map[string]*os.File
	cmds       map[string]*exec.Cmd
	lastActive time.Time
}

var (
	sessions   = make(map[string]*Session)
	sessionsMu sync.Mutex
	upgrader   = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	sessionTimeout = 10 * time.Minute // Session timeout
)

func main() {
	http.HandleFunc("/", indexHandler)
	http.HandleFunc("/ws", wsHandler)
	http.HandleFunc("/create_session", createSessionHandler)
	http.HandleFunc("/close_session", closeSessionHandler)

	// Start a goroutine for periodic cleanup of inactive sessions
	go sessionCleaner()

	fmt.Println("Server started on port :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// indexHandler handles the root route and returns the HTML page
func indexHandler(w http.ResponseWriter, r *http.Request) {
	html, err := os.ReadFile("index.html")
	if err != nil {
		http.Error(w, "Error reading HTML file", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(html)
}

// createSessionHandler creates a new session and returns the sessionID
func createSessionHandler(w http.ResponseWriter, r *http.Request) {
	session, err := createSession()
	if err != nil {
		log.Println("Error creating session:", err)
		http.Error(w, "Error creating session", http.StatusInternalServerError)
		return
	}
	// Return sessionID in JSON response
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"sessionID": session.hash})
}

// closeSessionHandler terminates the session and cleans up resources
func closeSessionHandler(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("sessionID")
	if sessionID == "" {
		http.Error(w, "Missing sessionID", http.StatusBadRequest)
		return
	}

	sessionsMu.Lock()
	session, exists := sessions[sessionID]
	if !exists {
		sessionsMu.Unlock()
		http.Error(w, "Session not found", http.StatusNotFound)
		return
	}
	delete(sessions, sessionID)
	sessionsMu.Unlock()

	cleanupSession(session)
	log.Printf("Session %s terminated by client request", sessionID)
	w.WriteHeader(http.StatusOK)
}

// wsHandler handles WebSocket connections
func wsHandler(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("sessionID")
	machineID := r.URL.Query().Get("machine")

	if sessionID == "" {
		http.Error(w, "Missing sessionID", http.StatusBadRequest)
		return
	}

	if machineID != "1" && machineID != "2" {
		http.Error(w, "Invalid machine ID", http.StatusBadRequest)
		return
	}

	sessionsMu.Lock()
	session := sessions[sessionID]
	if session == nil {
		sessionsMu.Unlock()
		http.Error(w, "Session not found", http.StatusNotFound)
		return
	}
	// Update the last activity time of the session
	session.lastActive = time.Now()
	sessionsMu.Unlock()

	// Establish WebSocket connection
	wsConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Error upgrading to WebSocket:", err)
		return
	}
	defer wsConn.Close()

	ptmx, ok := session.ptyFiles[machineID]
	if !ok {
		log.Println("Invalid machine ID:", machineID)
		wsConn.WriteMessage(websocket.TextMessage, []byte("Invalid machine ID"))
		return
	}

	// Read from PTY and send to WebSocket
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := ptmx.Read(buf)
			if err != nil {
				log.Println("Error reading from PTY:", err)
				wsConn.Close()
				break
			}
			if err := wsConn.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
				log.Println("Error writing to WebSocket:", err)
				break
			}
		}
	}()

	// Read from WebSocket and write to PTY
	for {
		messageType, msg, err := wsConn.ReadMessage()
		if err != nil {
			log.Println("Error reading from WebSocket:", err)
			break
		}
		if messageType == websocket.BinaryMessage || messageType == websocket.TextMessage {
			if _, err := ptmx.Write(msg); err != nil {
				log.Println("Error writing to machine PTY:", err)
				break
			}
		}

		sessionsMu.Lock()
		session.lastActive = time.Now()
		sessionsMu.Unlock()
	}
}

// createSession creates a new session: generates a hash, sets up the network, and starts VMs
func createSession() (*Session, error) {
	hash, err := generateShortHash(6)
	if err != nil {
		return nil, fmt.Errorf("failed to generate hash: %v", err)
	}

	bridgeName := fmt.Sprintf("br-%s", hash)
	tap1Name := fmt.Sprintf("tap1-%s", hash)
	tap2Name := fmt.Sprintf("tap2-%s", hash)

	if len(bridgeName) > 15 || len(tap1Name) > 15 || len(tap2Name) > 15 {
		return nil, fmt.Errorf("interface name too long: %s, %s, %s", bridgeName, tap1Name, tap2Name)
	}

	session := &Session{
		hash:       hash,
		bridgeName: bridgeName,
		tapNames:   map[string]string{"1": tap1Name, "2": tap2Name},
		ptyFiles:   make(map[string]*os.File),
		cmds:       make(map[string]*exec.Cmd),
		lastActive: time.Now(), // Set the session creation time
	}

	if err := setupNetwork(session); err != nil {
		return nil, fmt.Errorf("failed to set up network: %v", err)
	}

	if err := startMachine(session, "1", tap1Name); err != nil {
		cleanupNetwork(session)
		return nil, fmt.Errorf("failed to start machine 1: %v", err)
	}
	if err := startMachine(session, "2", tap2Name); err != nil {
		cleanupNetwork(session)
		return nil, fmt.Errorf("failed to start machine 2: %v", err)
	}

	// Add the session to the global map
	sessionsMu.Lock()
	sessions[hash] = session
	sessionsMu.Unlock()

	log.Printf("Session %s created\n", hash)
	return session, nil
}

// cleanupSession cleans up session resources: terminates VMs and removes interfaces
func cleanupSession(session *Session) {
	// Terminate virtual machines
	for id, cmd := range session.cmds {
		if cmd != nil && cmd.Process != nil {
			if err := cmd.Process.Kill(); err != nil {
				log.Printf("Error terminating machine %s: %v", id, err)
			} else {
				log.Printf("Machine %s in session %s terminated", id, session.hash)
			}
		}
	}

	// Close PTYs
	for _, pt := range session.ptyFiles {
		if pt != nil {
			pt.Close()
		}
	}

	// Clean up the network
	if err := cleanupNetwork(session); err != nil {
		log.Printf("Error cleaning up network for session %s: %v", session.hash, err)
	} else {
		log.Printf("Network for session %s cleaned up", session.hash)
	}

	log.Printf("Session %s removed\n", session.hash)
}

// sessionCleaner periodically checks and cleans up inactive sessions
func sessionCleaner() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		sessionsMu.Lock()
		for id, session := range sessions {
			if time.Since(session.lastActive) > sessionTimeout {
				log.Printf("Session %s inactive for more than %v and will be removed", id, sessionTimeout)
				delete(sessions, id)
				go cleanupSession(session)
			}
		}
		sessionsMu.Unlock()
	}
}

// generateShortHash generates a short hash of the specified length
func generateShortHash(length int) (string, error) {
	if length > 12 {
		length = 12
	}
	bytes := make([]byte, length/2)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// setupNetwork configures network interfaces for the session
func setupNetwork(session *Session) error {
	if exists, err := interfaceExists(session.bridgeName); err != nil {
		return fmt.Errorf("error checking existence of bridge %s: %v", session.bridgeName, err)
	} else if exists {
		log.Printf("Bridge %s already exists. Deleting...", session.bridgeName)
		if err := runCommand("ip", "link", "delete", session.bridgeName, "type", "bridge"); err != nil {
			return fmt.Errorf("failed to delete bridge %s: %v", session.bridgeName, err)
		}
	}

	log.Printf("Creating bridge %s...", session.bridgeName)
	if err := runCommand("ip", "link", "add", session.bridgeName, "type", "bridge"); err != nil {
		return fmt.Errorf("failed to create bridge %s: %v", session.bridgeName, err)
	}

	log.Printf("Bringing up bridge %s...", session.bridgeName)
	if err := runCommand("ip", "link", "set", session.bridgeName, "up"); err != nil {
		return fmt.Errorf("failed to bring up bridge %s: %v", session.bridgeName, err)
	}

	for _, tap := range session.tapNames {
		log.Printf("Creating TAP device %s...", tap)
		if err := runCommand("ip", "tuntap", "add", "mode", "tap", tap); err != nil {
			return fmt.Errorf("failed to create TAP device %s: %v", tap, err)
		}

		log.Printf("Attaching TAP device %s to bridge %s...", tap, session.bridgeName)
		if err := runCommand("ip", "link", "set", tap, "master", session.bridgeName); err != nil {
			return fmt.Errorf("failed to attach TAP device %s to bridge %s: %v", tap, session.bridgeName, err)
		}

		log.Printf("Bringing up TAP device %s...", tap)
		if err := runCommand("ip", "link", "set", tap, "up"); err != nil {
			return fmt.Errorf("failed to bring up TAP device %s: %v", tap, err)
		}
	}

	log.Printf("Network setup for session %s completed successfully.", session.hash)
	return nil
}

// cleanupNetwork removes the session's network interfaces
func cleanupNetwork(session *Session) error {
	commands := [][]string{
		{"ip", "link", "set", session.bridgeName, "down"},
		{"ip", "link", "delete", session.bridgeName, "type", "bridge"},
	}

	for _, tap := range session.tapNames {
		commands = append(commands, []string{"ip", "link", "set", tap, "down"})
		commands = append(commands, []string{"ip", "link", "delete", tap})
	}

	for _, cmdArgs := range commands {
		if err := runCommand(cmdArgs...); err != nil {
			if strings.Contains(err.Error(), "Cannot find device") || strings.Contains(err.Error(), "No such device") {
				continue
			}
			log.Printf("Error executing cleanup command %v: %v", cmdArgs, err)
		} else {
			log.Printf("Successfully executed cleanup command: %v", cmdArgs)
		}
	}

	return nil
}

// interfaceExists checks if a network interface with the given name exists
func interfaceExists(name string) (bool, error) {
	cmd := exec.Command("ip", "link", "show", name)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(output), "does not exist") ||
			strings.Contains(string(output), "Cannot find device") ||
			strings.Contains(string(output), "No such device") {
			return false, nil
		}
		return false, fmt.Errorf("error executing 'ip link show %s': %v, output: %s", name, err, string(output))
	}
	return true, nil // Interface exists
}

// runCommand executes a system command and returns an error if it occurred
func runCommand(args ...string) error {
	cmd := exec.Command(args[0], args[1:]...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("command '%s' failed: %v, output: %s", strings.Join(args, " "), err, string(output))
	}
	return nil
}

// startMachine launches a virtual machine with connects it to the TAP device
func startMachine(session *Session, machineID string, tapDevice string) error {
	netDevID := fmt.Sprintf("net%s", machineID)
	cmd := exec.Command("qemu-system-x86_64",
		"-accel", "kvm",
		"-drive", fmt.Sprintf("file=debian-12-nocloud-amd64.qcow2,format=qcow2,if=virtio"),
		"-display", "none",
		"-netdev", fmt.Sprintf("tap,ifname=%s,id=%s,script=no,downscript=no", tapDevice, netDevID),
		"-device", fmt.Sprintf("virtio-net-pci,netdev=%s,mac=e6:c8:ff:09:76:%02x", netDevID, 100+machineID[0]),
		"-chardev", "stdio,id=char0,signal=off",
		"-serial", "chardev:char0",
		"-m", "256",
		"-snapshot",
		"-sandbox", "on",
	)

	// Start QEMU and get the PTY connected to its stdin/stdout
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return fmt.Errorf("error starting QEMU machine %s: %v", machineID, err)
	}

	session.ptyFiles[machineID] = ptmx
	session.cmds[machineID] = cmd

	log.Printf("Virtual machine %s in session %s started\n", machineID, session.hash)
	return nil
}
