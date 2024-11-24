# Web Terminal for Virtual Machines

This project on Go implements a web-based terminal for interacting with virtual machines (VMs) using WebSockets. It allows users to connect to and control two virtual machines in a session through a web interface.

## Key Features:
- **Web Terminal**: Uses `xterm.js` for terminal emulation in the browser.
- **Session Management**: Supports creating, managing, and closing sessions.
- **Machine Interaction**: Users can connect to two virtual machines (VM1 and VM2) running on the server.
- **WebSocket Communication**: Provides real-time, bidirectional communication between the browser and the server, sending and receiving terminal data.
- **Network Configuration**: Dynamically creates and manages virtual network interfaces (TAP devices) for each session and VM.

## How It Works:
1. A session is created by calling the `/create_session` endpoint, generating a unique session ID.
2. Users can connect to either VM1 or VM2 through WebSocket, with terminal data sent back and forth.
3. The session is automatically cleaned up after inactivity or when the user navigates away from the page.

## Use Cases:
This project is useful for scenarios where you need to provide terminal access to virtual machines remotely, such as:
- Cloud-based development environments
- Remote server management
- Testing and automation platforms
