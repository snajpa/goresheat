package main

import (
	"bufio"
	"encoding/csv"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

type CPUUsage struct {
	User float64
	Sys  float64
	Idle float64
}

type CPUCore struct {
	Node  int
	HTID  int
	Usage CPUUsage
}

var (
	usageHistory  = make([]string, 0)
	historyMutex  sync.Mutex
	clients       = make(map[*websocket.Conn]bool)
	clientsMutex  sync.Mutex
	previousUsage []CPUCore
	interval      time.Duration
	host          string
	port          string
	historyLength int
	rectSize      = 20
	cpuCores      = make(map[int][]CPUCore) // Map of NUMA nodes to cores
	numCPUs       int
)

// readCPUUsage reads the CPU usage from the /proc/stat file.
func readCPUUsage() ([]CPUCore, error) {
	file, err := os.Open("/proc/stat")
	if err != nil {
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	var cores []CPUCore
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "cpu") && line[3] != ' ' {
			fields := strings.Fields(line)[1:]
			user, _ := strconv.ParseInt(fields[0], 10, 64)
			sys, _ := strconv.ParseInt(fields[2], 10, 64)
			idle, _ := strconv.ParseInt(fields[3], 10, 64)
			cores = append(cores, CPUCore{
				Node:  0, // Will be updated later
				HTID:  0, // Will be updated later
				Usage: CPUUsage{User: float64(user), Sys: float64(sys), Idle: float64(idle)},
			})
		}
	}
	return cores, nil
}

// calculatePercentage calculates the percentage difference in CPU usage between two reads
func calculatePercentage(current, previous []CPUCore) []CPUCore {
	if len(previous) == 0 {
		return current
	}

	var result []CPUCore
	for i := range current {
		total := (current[i].Usage.User + current[i].Usage.Sys + current[i].Usage.Idle) -
			(previous[i].Usage.User + previous[i].Usage.Sys + previous[i].Usage.Idle)

		if total > 0 {
			result = append(result, CPUCore{
				Node: current[i].Node,
				HTID: current[i].HTID,
				Usage: CPUUsage{
					User: (current[i].Usage.User - previous[i].Usage.User) / total * 100,
					Sys:  (current[i].Usage.Sys - previous[i].Usage.Sys) / total * 100,
					Idle: (current[i].Usage.Idle - previous[i].Usage.Idle) / total * 100,
				},
			})
		} else {
			result = append(result, CPUCore{
				Node: current[i].Node,
				HTID: current[i].HTID,
				Usage: CPUUsage{
					User: 0,
					Sys:  0,
					Idle: 0,
				},
			})
		}
	}
	return result
}

// handleConnections handles incoming WebSocket connections and sends current CPU usage data.
func handleConnections(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		fmt.Println("Error upgrading to WebSocket:", err)
		return
	}
	defer conn.Close()

	clientsMutex.Lock()
	clients[conn] = true
	clientsMutex.Unlock()

	for {
		time.Sleep(interval)
	}
}

// handleHistory serves the usage history to the clients.
func handleHistory(w http.ResponseWriter, r *http.Request) {
	historyMutex.Lock()
	defer historyMutex.Unlock()

	w.Header().Set("Content-Type", "text/csv")
	writer := csv.NewWriter(w)
	defer writer.Flush()

	for _, record := range usageHistory {
		writer.Write(strings.Split(record, ","))
	}
}

// broadcastUsage sends the current CPU usage to all connected clients.
func broadcastUsage() {
	for {
		time.Sleep(interval)

		currentUsage, err := readCPUUsage()
		if err != nil {
			fmt.Println("Error reading CPU usage:", err)
			return
		}

		percentageUsage := calculatePercentage(currentUsage, previousUsage)
		previousUsage = currentUsage

		// Get current time with milliseconds
		currentTime := time.Now().Format("15:04:05.000")

		// Convert usage to CSV format using csv.Writer
		var csvData [][]string
		header := []string{currentTime, strconv.Itoa(numCPUs)}

		// Add user percentage values
		for _, core := range percentageUsage {
			header = append(header, strconv.FormatFloat(core.Usage.User, 'f', 2, 64))
		}
		header = append(header, "", "") // background-colored spaces

		// Add sys percentage values
		for _, core := range percentageUsage {
			header = append(header, strconv.FormatFloat(core.Usage.Sys, 'f', 2, 64))
		}
		header = append(header, "", "") // background-colored spaces

		// Add idle percentage values (inverted)
		for _, core := range percentageUsage {
			header = append(header, strconv.FormatFloat(core.Usage.Idle, 'f', 2, 64))
		}

		csvData = append(csvData, header)

		var csvString strings.Builder
		writer := csv.NewWriter(&csvString)
		writer.WriteAll(csvData)
		writer.Flush()

		csvOutput := csvString.String()

		// Store the usage in history
		historyMutex.Lock()
		usageHistory = append([]string{strings.TrimSpace(csvOutput)}, usageHistory...)
		if len(usageHistory) > historyLength { // Limit history to historyLength entries
			usageHistory = usageHistory[:historyLength]
		}
		historyMutex.Unlock()

		// Broadcast to all clients
		clientsMutex.Lock()
		for client := range clients {
			err := client.WriteMessage(websocket.TextMessage, []byte(csvOutput))
			if err != nil {
				client.Close()
				delete(clients, client)
			}
		}
		clientsMutex.Unlock()
	}
}

// getCPUInfo reads the CPU topology from /proc/cpuinfo and populates the cpuCores map.
func getCPUInfo() error {
	file, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	var currentCore CPUCore
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "processor") {
			fields := strings.Fields(line)
			processorID, _ := strconv.Atoi(fields[2])
			currentCore = CPUCore{HTID: processorID}
			numCPUs++
		} else if strings.HasPrefix(line, "physical id") {
			fields := strings.Fields(line)
			nodeID, _ := strconv.Atoi(fields[3])
			currentCore.Node = nodeID
		} else if line == "" {
			cpuCores[currentCore.Node] = append(cpuCores[currentCore.Node], currentCore)
		}
	}
	return nil
}

func main() {
	flag.DurationVar(&interval, "interval", 100*time.Millisecond, "Update interval")
	flag.StringVar(&host, "host", "0.0.0.0", "Host address")
	flag.StringVar(&port, "port", "8080", "Port number")
	flag.IntVar(&historyLength, "history", 150, "History length")
	flag.IntVar(&rectSize, "rectsize", 9, "Rectangle size")
	flag.Parse()

	err := getCPUInfo()
	if err != nil {
		fmt.Println("Error reading CPU info:", err)
		return
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		intervalMsec := interval.Milliseconds()
		fmt.Fprintf(w, htmlContent, rectSize, historyLength, intervalMsec)
	})
	http.HandleFunc("/ws", handleConnections)
	http.HandleFunc("/history", handleHistory)
	go broadcastUsage()
	address := fmt.Sprintf("%s:%s", host, port)
	fmt.Println("Listening on", address)
	http.ListenAndServe(address, nil)
}

const htmlContent = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <title>CPU Load Monitor</title>
    <style>
        body {
            background-color: #16273f;
            margin: 0;
            overflow: hidden; /* Prevent scrolling */
        }
        canvas {
            border: 1px solid #16273f;
            background-color: #16273f;
            position: fixed; /* Fix the canvas position */
            top: 0;
            left: 0;
        }
    </style>
</head>
<body>
    <canvas id="cpuCanvas"></canvas>
    <script>
        document.addEventListener("DOMContentLoaded", function () {
            const socket = new WebSocket('ws://' + window.location.host + '/ws');
            const canvas = document.getElementById('cpuCanvas');
            const ctx = canvas.getContext('2d');
            const rectSize = %d;
            const historyLength = %d;
            const timeWidth = rectSize * 9; // Width allocated for the time labels
            const dataInterval = %d;
            const timerSlack = dataInterval * 0.2; // Slack interval in milliseconds
            const slackInterval = dataInterval - timerSlack; // Interval considering slack
            const maxHistoryLength = historyLength; // Maximum length of the history array
            let history = [];
            let isPaused = false;
            let scrollOffset = 0;
            let animationFrameId;
            let lastUpdateTime = performance.now();
            const scrollSpeed = rectSize / dataInterval; // Scroll speed calculation
            let dataQueue = []; // Queue to buffer incoming data points
            let lastDataTimestamp = performance.now(); // Timestamp of the last data row
            let targetRedrawInterval = dataInterval; // Start with the default interval
            let redrawStartTime;

            const offScreenCanvas = document.createElement('canvas');
            const offScreenCtx = offScreenCanvas.getContext('2d');

            function fetchHistory() {
                fetch('/history')
                    .then(response => response.text())
                    .then(data => {
                        history = [];
                        data.split('\n').forEach(row => {
                            row = row.trim();
                            if (row) {
                                history.push(row.split(','));
                            }
                        });
                        if (history.length > maxHistoryLength) {
                            history = history.slice(0, maxHistoryLength);
                        }
                        updateCanvasDimensions();
                        draw();
                    })
                    .catch(error => {
                        console.error('Fetching history failed:', error);
                    });
            }

            function updateCanvasDimensions() {
                if (history.length > 0) {
                    canvas.width = (history[0].length - 2) * rectSize + timeWidth;
                    canvas.height = historyLength * rectSize;
                    offScreenCanvas.width = canvas.width;
                    offScreenCanvas.height = canvas.height;
                }
            }

            function getFlameColor(percentage) {
                if (percentage <= 5) {
                    return 'rgb(0,0,0)';
                } else if (percentage <= 20) {
                    return 'rgb(50,20,0)';
                } else if (percentage <= 40) {
                    return 'rgb(139,0,0)';
                } else if (percentage <= 60) {
                    return 'rgb(255,69,0)';
                } else if (percentage <= 80) {
                    return 'rgb(255,140,0)';
                } else if (percentage <= 95) {
                    return 'rgb(255,255,0)';
                } else {
                    return 'rgb(255,255,255)';
                }
            }

            function draw() {
                if (history.length === 0) return;

                redrawStartTime = performance.now();
                const elapsed = redrawStartTime - lastUpdateTime;
                lastUpdateTime = redrawStartTime;

                scrollOffset += (scrollSpeed * elapsed);

                if (scrollOffset >= rectSize) {
                    scrollOffset -= rectSize;

                    if (dataQueue.length > 0) {
                        const newData = dataQueue.shift();
                        history.unshift(newData);
                    } else {
                        const lastRow = history.length > 0 ? history[0].slice() : [];
                        history.unshift(lastRow);
                    }

                    if (history.length > historyLength) {
                        history.pop();
                    }
                }

                offScreenCtx.clearRect(0, 0, offScreenCanvas.width, offScreenCanvas.height);
                offScreenCtx.font = (rectSize) + 'px monospace';

                const startRow = Math.max(0, history.length - historyLength);
                const displayedHistory = history.slice(startRow, history.length);

                offScreenCtx.save();
                offScreenCtx.translate(0, Math.floor(scrollOffset));

                displayedHistory.forEach((data, row) => {
                    let coreIndex = 0;
                    const yOffset = Math.floor(row * rectSize);

                    if (yOffset + rectSize <= 0 || yOffset >= canvas.height) {
                        return;
                    }

                    data.forEach((value, col) => {
                        if (col === 0) {
                            const x = 2;
                            const y = Math.floor(yOffset + rectSize - 2);
                            if (value) {
                                offScreenCtx.fillStyle = '#FFFFFF';
                                try {
                                    offScreenCtx.fillText(value.padEnd(12, '0'), x, y);
                                } catch (e) {
                                    console.error('Error drawing text:', e);
                                }
                            }
                        } else if (col === 1) {
                            return;
                        } else {
                            if (value === "" || value === "|") {
                                coreIndex++;
                                return;
                            }
                            const x = Math.floor(coreIndex * rectSize + timeWidth);
                            const y = Math.floor(yOffset);
                            let color = getFlameColor(parseFloat(value));
                            offScreenCtx.fillStyle = color;
                            offScreenCtx.fillRect(x, y, rectSize, rectSize);
                            coreIndex++;
                        }
                    });
                });

                offScreenCtx.restore();

                ctx.clearRect(0, 0, canvas.width, canvas.height);
                ctx.drawImage(offScreenCanvas, 0, 0);

                const redrawTime = performance.now() - redrawStartTime;
                if (redrawTime > targetRedrawInterval) {
                    targetRedrawInterval = redrawTime * 1.5; // Increase the interval more aggressively
                } else {
                    targetRedrawInterval = targetRedrawInterval * 0.8 + redrawTime * 0.2; // Adjust down more smoothly
                }

                if (!isPaused) {
                    setTimeout(() => {
                        animationFrameId = requestAnimationFrame(draw);
                    }, targetRedrawInterval);
                }
            }

            function handleVisibilityChange() {
                if (document.visibilityState === 'visible') {
                    fetchHistory();
                    lastUpdateTime = performance.now();
                    scrollOffset = 0;
                    if (!isPaused) {
                        animationFrameId = requestAnimationFrame(draw);
                    }
                } else {
                    cancelAnimationFrame(animationFrameId);
                }
            }

            function togglePause() {
                isPaused = !isPaused;
                console.log("Paused:", isPaused);
                if (!isPaused) {
                    fetchHistory();
                    lastUpdateTime = performance.now();
                    animationFrameId = requestAnimationFrame(draw);
                } else {
                    cancelAnimationFrame(animationFrameId);
                }
            }

            function refreshHistoryPeriodically() {
                setInterval(fetchHistory, 60000);
            }

            function clearDataQueuePeriodically() {
                setInterval(() => {
                    if (dataQueue.length > maxHistoryLength) {
                        dataQueue = dataQueue.slice(-maxHistoryLength);
                    }
                }, 60000); // Clear the data queue every 60 seconds
            }

            document.addEventListener("keydown", togglePause);
            canvas.addEventListener("click", togglePause);
            document.addEventListener("visibilitychange", handleVisibilityChange);

            updateCanvasDimensions();
            fetchHistory();
            refreshHistoryPeriodically();
            clearDataQueuePeriodically();

            socket.onopen = function() {
                console.log('WebSocket connection opened.');
            };

            socket.onmessage = function(event) {
                if (isPaused) return;

                const data = event.data.split(",");
                if (data.length > 0) {
                    dataQueue.push(data);
                }
            };

            socket.onerror = function(error) {
                console.error('WebSocket error:', error);
            };

            socket.onclose = function() {
                console.log('WebSocket connection closed.');
            };

            animationFrameId = requestAnimationFrame(draw);
        });
    </script>
</body>
</html>
`
