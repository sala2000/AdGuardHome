package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/AdguardTeam/AdguardDNS/dnsfilter"
	"gopkg.in/asaskevich/govalidator.v4"
)

const updatePeriod = time.Minute * 30

var coreDNSCommand *exec.Cmd

var filterTitle = regexp.MustCompile(`^! Title: +(.*)$`)

// -------------------
// coredns run control
// -------------------
func tellCoreDNSToReload() {
	// not running -- cheap check
	if coreDNSCommand == nil && coreDNSCommand.Process == nil {
		return
	}
	// not running -- more expensive check
	if !isRunning() {
		return
	}

	pid := coreDNSCommand.Process.Pid
	process, err := os.FindProcess(pid)
	if err != nil {
		log.Printf("os.FindProcess(%d) returned err: %v\n", pid, err)
		return
	}
	err = process.Signal(syscall.SIGUSR1)
	if err != nil {
		log.Printf("process.Signal on pid %d returned: %v\n", pid, err)
		return
	}
}

func writeAllConfigsAndReloadCoreDNS() error {
	err := writeAllConfigs()
	if err != nil {
		log.Printf("Couldn't write all configs: %s", err)
		return err
	}
	tellCoreDNSToReload()
	return nil
}

func isRunning() bool {
	if coreDNSCommand != nil && coreDNSCommand.Process != nil {
		pid := coreDNSCommand.Process.Pid
		process, err := os.FindProcess(pid)
		if err != nil {
			log.Printf("os.FindProcess(%d) returned err: %v\n", pid, err)
		} else {
			err := process.Signal(syscall.Signal(0))
			if err != nil {
				log.Printf("process.Signal on pid %d returned: %v\n", pid, err)
			}
			if err == nil {
				return true
			}
		}
	}
	return false
}

func startDNSServer() error {
	if isRunning() {
		return fmt.Errorf("Unable to start coreDNS: Already running")
	}
	err := writeCoreDNSConfig()
	if err != nil {
		errortext := fmt.Errorf("Unable to write coredns config: %s", err)
		log.Println(errortext)
		return errortext
	}
	err = writeFilterFile()
	if err != nil {
		errortext := fmt.Errorf("Couldn't write filter file: %s", err)
		log.Println(errortext)
		return errortext
	}
	binarypath := filepath.Join(config.ourBinaryDir, config.CoreDNS.binaryFile)
	configpath := filepath.Join(config.ourBinaryDir, config.CoreDNS.coreFile)
	coreDNSCommand = exec.Command(binarypath, "-conf", configpath, "-dns.port", fmt.Sprintf("%d", config.CoreDNS.Port))
	coreDNSCommand.Stdout = os.Stdout
	coreDNSCommand.Stderr = os.Stderr
	err = coreDNSCommand.Start()
	if err != nil {
		errortext := fmt.Errorf("Unable to start coreDNS: %s", err)
		log.Println(errortext)
		return errortext
	}
	log.Printf("coredns PID: %v\n", coreDNSCommand.Process.Pid)
	go childwaiter()
	return nil
}

func handleStart(w http.ResponseWriter, r *http.Request) {
	if isRunning() {
		http.Error(w, fmt.Sprintf("Unable to start coreDNS: Already running"), 400)
		return
	}
	err := startDNSServer()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	fmt.Fprintf(w, "OK, PID %d\n", coreDNSCommand.Process.Pid)
}

func childwaiter() {
	err := coreDNSCommand.Wait()
	log.Printf("coredns terminated: %s\n", err)
	err = coreDNSCommand.Process.Release()
	log.Printf("coredns released: %s\n", err)
}

func handleStop(w http.ResponseWriter, r *http.Request) {
	if coreDNSCommand == nil || coreDNSCommand.Process == nil {
		http.Error(w, fmt.Sprintf("Unable to start coreDNS: Not running"), 400)
		return
	}
	if isRunning() {
		http.Error(w, fmt.Sprintf("Unable to start coreDNS: Not running"), 400)
		return
	}
	cmd := coreDNSCommand
	// TODO: send SIGTERM first, then SIGKILL
	err := cmd.Process.Kill()
	if err != nil {
		errortext := fmt.Sprintf("Unable to stop coreDNS:\nGot error %T\n%v\n%s", err, err, err)
		log.Println(errortext)
		http.Error(w, errortext, 500)
		return
	}
	exitstatus := cmd.Wait()

	err = cmd.Process.Release()
	if err != nil {
		errortext := fmt.Sprintf("Unable to release process resources: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, 500)
		return
	}
	// this err is ignorable, it shows exit status of coredns
	fmt.Fprintf(w, "OK\n%s\n", exitstatus)
}

func handleRestart(w http.ResponseWriter, r *http.Request) {
	handleStop(w, r)
	handleStart(w, r)
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	data := map[string]interface{}{
		"running":          isRunning(),
		"version":          VersionString,
		"dns_address":      config.BindHost,
		"dns_port":         config.CoreDNS.Port,
		"querylog_enabled": config.CoreDNS.QueryLogEnabled,
	}

	json, err := json.Marshal(data)
	if err != nil {
		errortext := fmt.Sprintf("Unable to marshal status json: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, err = w.Write(json)
	if err != nil {
		errortext := fmt.Sprintf("Unable to write response json: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, 500)
		return
	}
}

// -----
// stats
// -----
func handleStats(w http.ResponseWriter, r *http.Request) {
	histrical := generateMapFromStats(&statistics.perMinute, 0, 2)
	// sum them up
	summed := map[string]interface{}{}
	for key, values := range histrical {
		summedValue := 0.0
		floats, ok := values.([]float64)
		if !ok {
			continue
		}
		for _, v := range floats {
			summedValue += v
		}
		summed[key] = summedValue
	}
	summed["stats_period"] = "3 minutes"

	json, err := json.Marshal(summed)
	if err != nil {
		errortext := fmt.Sprintf("Unable to marshal status json: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, err = w.Write(json)
	if err != nil {
		errortext := fmt.Sprintf("Unable to write response json: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, 500)
		return
	}
}

func handleStatsHistory(w http.ResponseWriter, r *http.Request) {
	// handle time unit and prepare our time window size
	now := time.Now()
	timeUnitString := r.URL.Query().Get("time_unit")
	var stats *periodicStats
	var timeUnit time.Duration
	switch timeUnitString {
	case "seconds":
		timeUnit = time.Second
		stats = &statistics.perSecond
	case "minutes":
		timeUnit = time.Minute
		stats = &statistics.perMinute
	case "hours":
		timeUnit = time.Hour
		stats = &statistics.perHour
	case "days":
		timeUnit = time.Hour * 24
		stats = &statistics.perDay
	default:
		http.Error(w, "Must specify valid time_unit parameter", 400)
		return
	}

	// parse start and end time
	startTime, err := time.Parse(time.RFC3339, r.URL.Query().Get("start_time"))
	if err != nil {
		errortext := fmt.Sprintf("Must specify valid start_time parameter: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, 400)
		return
	}
	endTime, err := time.Parse(time.RFC3339, r.URL.Query().Get("end_time"))
	if err != nil {
		errortext := fmt.Sprintf("Must specify valid end_time parameter: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, 400)
		return
	}

	// check if start and time times are within supported time range
	timeRange := timeUnit * statsHistoryElements
	if startTime.Add(timeRange).Before(now) {
		http.Error(w, "start_time parameter is outside of supported range", 501)
		return
	}
	if endTime.Add(timeRange).Before(now) {
		http.Error(w, "end_time parameter is outside of supported range", 501)
		return
	}

	// calculate start and end of our array
	// basically it's how many hours/minutes/etc have passed since now
	start := int(now.Sub(endTime) / timeUnit)
	end := int(now.Sub(startTime) / timeUnit)

	// swap them around if they're inverted
	if start > end {
		start, end = end, start
	}

	data := generateMapFromStats(stats, start, end)
	json, err := json.Marshal(data)
	if err != nil {
		errortext := fmt.Sprintf("Unable to marshal status json: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, err = w.Write(json)
	if err != nil {
		errortext := fmt.Sprintf("Unable to write response json: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, 500)
		return
	}
}

func handleQueryLog(w http.ResponseWriter, r *http.Request) {
	isDownload := r.URL.Query().Get("download") != ""
	resp, err := client.Get("http://127.0.0.1:8618/querylog")
	if err != nil {
		errortext := fmt.Sprintf("Couldn't get querylog from coredns: %T %s\n", err, err)
		log.Println(errortext)
		http.Error(w, errortext, http.StatusBadGateway)
		return
	}
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}

	// read the body entirely
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		errortext := fmt.Sprintf("Couldn't read response body: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, http.StatusBadGateway)
		return
	}

	// forward body entirely with status code
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if isDownload {
		w.Header().Set("Content-Disposition", "attachment; filename=querylog.json")
	}
	w.WriteHeader(resp.StatusCode)
	_, err = w.Write(body)
	if err != nil {
		errortext := fmt.Sprintf("Couldn't write body: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, http.StatusInternalServerError)
	}
}

func handleQueryLogEnable(w http.ResponseWriter, r *http.Request) {
	config.CoreDNS.QueryLogEnabled = true
	err := writeAllConfigsAndReloadCoreDNS()
	if err != nil {
		errortext := fmt.Sprintf("Couldn't write config file: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, http.StatusInternalServerError)
		return
	}
	fmt.Fprintf(w, "OK\n")
}

func handleQueryLogDisable(w http.ResponseWriter, r *http.Request) {
	config.CoreDNS.QueryLogEnabled = false
	err := writeAllConfigsAndReloadCoreDNS()
	if err != nil {
		errortext := fmt.Sprintf("Couldn't write config file: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, http.StatusInternalServerError)
		return
	}
	fmt.Fprintf(w, "OK\n")
}

func handleStatsTop(w http.ResponseWriter, r *http.Request) {
	resp, err := client.Get("http://127.0.0.1:8618/querylog")
	if err != nil {
		errortext := fmt.Sprintf("Couldn't get querylog from coredns: %T %s\n", err, err)
		log.Println(errortext)
		http.Error(w, errortext, http.StatusBadGateway)
		return
	}
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}

	// read body
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		errortext := fmt.Sprintf("Couldn't read response body: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, http.StatusBadGateway)
		return
	}
	// empty body
	if len(body) == 0 {
		return
	}

	values := []interface{}{}
	err = json.Unmarshal(body, &values)
	if err != nil {
		errortext := fmt.Sprintf("Couldn't parse response body: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, http.StatusBadGateway)
		return
	}

	domains := map[string]int{}
	blocked := map[string]int{}
	clients := map[string]int{}
	now := time.Now()
	timeWindow := time.Minute * 3
	notBefore := now.Add(timeWindow * -1)

	for _, value := range values {
		entry, ok := value.(map[string]interface{})
		if !ok {
			// ignore anything else
			continue
		}
		host := getHost(entry)
		reason := getReason(entry)
		client := getClient(entry)
		time := getTime(entry)
		if time.Before(notBefore) {
			// skip if the entry is before specified cutoff
			continue
		}
		if len(host) > 0 {
			domains[host]++
		}
		if len(host) > 0 && strings.HasPrefix(reason, "Filtered") {
			blocked[host]++
		}
		if len(client) > 0 {
			clients[client]++
		}
	}

	// use manual json marshalling because we want maps to be sorted by value
	json := bytes.Buffer{}
	json.WriteString("{\n")

	gen := func(json *bytes.Buffer, name string, top map[string]int, addComma bool) {
		json.WriteString("  \"")
		json.WriteString(name)
		json.WriteString("\": {\n")
		sorted := sortByValue(top)
		for i, key := range sorted {
			fmt.Fprintf(json, "    \"%s\": %d", key, top[key])
			if i+1 != len(sorted) {
				json.WriteByte(',')
			}
			json.WriteByte('\n')
		}
		json.WriteString("  }")
		if addComma {
			json.WriteByte(',')
		}
		json.WriteByte('\n')
	}
	gen(&json, "top_queried_domains", domains, true)
	gen(&json, "top_blocked_domains", blocked, true)
	gen(&json, "top_clients", clients, false)
	json.WriteString("}\n")

	w.Header().Set("Content-Type", "application/json")
	_, err = w.Write(json.Bytes())
	if err != nil {
		errortext := fmt.Sprintf("Couldn't write body: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, http.StatusInternalServerError)
	}
}

func handleSetUpstreamDNS(w http.ResponseWriter, r *http.Request) {
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		errortext := fmt.Sprintf("Failed to read request body: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, 400)
		return
	}
	// if empty body -- user is asking for default servers
	hosts := parseIPsOptionalPort(string(body))
	if len(hosts) == 0 {
		config.CoreDNS.UpstreamDNS = defaultDNS
	} else {
		config.CoreDNS.UpstreamDNS = hosts
	}

	err = writeAllConfigs()
	if err != nil {
		errortext := fmt.Sprintf("Couldn't write config file: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, http.StatusInternalServerError)
		return
	}
	tellCoreDNSToReload()
	fmt.Fprintf(w, "OK %d servers\n", len(hosts))
}

func parseIPsOptionalPort(input string) []string {
	fields := strings.Fields(input)
	hosts := []string{}
	for _, field := range fields {
		_, _, err := net.SplitHostPort(field)
		if err != nil {
			ip := net.ParseIP(field)
			if ip == nil {
				log.Printf("Invalid DNS server field: %s\n", field)
				continue
			}
		}
		hosts = append(hosts, field)
	}
	return hosts
}

// ---------
// filtering
// ---------

func handleFilteringEnable(w http.ResponseWriter, r *http.Request) {
	config.CoreDNS.FilteringEnabled = true
	err := writeAllConfigsAndReloadCoreDNS()
	if err != nil {
		errortext := fmt.Sprintf("Couldn't write config file: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, http.StatusInternalServerError)
		return
	}
	fmt.Fprintf(w, "OK\n")
}

func handleFilteringDisable(w http.ResponseWriter, r *http.Request) {
	config.CoreDNS.FilteringEnabled = false
	err := writeAllConfigsAndReloadCoreDNS()
	if err != nil {
		errortext := fmt.Sprintf("Couldn't write config file: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, http.StatusInternalServerError)
		return
	}
	fmt.Fprintf(w, "OK\n")
}

func handleFilteringStatus(w http.ResponseWriter, r *http.Request) {
	data := map[string]interface{}{
		"enabled": config.CoreDNS.FilteringEnabled,
	}

	data["filters"] = config.Filters
	data["user_rules"] = config.UserRules

	json, err := json.Marshal(data)

	if err != nil {
		errortext := fmt.Sprintf("Unable to marshal status json: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, 500)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, err = w.Write(json)
	if err != nil {
		errortext := fmt.Sprintf("Unable to write response json: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, 500)
		return
	}
}

func handleFilteringAddURL(w http.ResponseWriter, r *http.Request) {
	parameters, err := parseParametersFromBody(r.Body)
	if err != nil {
		errortext := fmt.Sprintf("failed to parse parameters from body: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, 400)
		return
	}

	url, ok := parameters["url"]
	if !ok {
		http.Error(w, "URL parameter was not specified", 400)
		return
	}

	if valid := govalidator.IsRequestURL(url); !valid {
		http.Error(w, "URL parameter is not valid request URL", 400)
		return
	}

	// check for duplicates
	for i := range config.Filters {
		filter := &config.Filters[i]
		if filter.URL == url {
			errortext := fmt.Sprintf("Filter URL already added -- %s", url)
			log.Println(errortext)
			http.Error(w, errortext, http.StatusBadRequest)
			return
		}
	}

	var filter = filter{
		Enabled: true,
		URL:     url,
	}
	ok, err = filter.update(time.Now())
	if err != nil {
		errortext := fmt.Sprintf("Couldn't fetch filter from url %s: %s", url, err)
		log.Println(errortext)
		http.Error(w, errortext, http.StatusBadRequest)
		return
	}
	if filter.RulesCount == 0 {
		errortext := fmt.Sprintf("Filter at url %s has no rules (maybe it points to blank page?)", url)
		log.Println(errortext)
		http.Error(w, errortext, http.StatusBadRequest)
		return
	}
	if !ok {
		errortext := fmt.Sprintf("Filter at url %s is invalid (maybe it points to blank page?)", url)
		log.Println(errortext)
		http.Error(w, errortext, http.StatusBadRequest)
		return
	}

	// URL is deemed valid, append it to filters, update config, write new filter file and tell coredns to reload it
	config.Filters = append(config.Filters, filter)
	err = writeAllConfigs()
	if err != nil {
		errortext := fmt.Sprintf("Couldn't write config file: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, http.StatusInternalServerError)
		return
	}
	err = writeFilterFile()
	if err != nil {
		errortext := fmt.Sprintf("Couldn't write filter file: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, http.StatusInternalServerError)
		return
	}
	tellCoreDNSToReload()
	fmt.Fprintf(w, "OK %d rules\n", filter.RulesCount)
}

func handleFilteringRemoveURL(w http.ResponseWriter, r *http.Request) {
	parameters, err := parseParametersFromBody(r.Body)
	if err != nil {
		errortext := fmt.Sprintf("failed to parse parameters from body: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, 400)
		return
	}

	url, ok := parameters["url"]
	if !ok {
		http.Error(w, "URL parameter was not specified", 400)
		return
	}

	if valid := govalidator.IsRequestURL(url); !valid {
		http.Error(w, "URL parameter is not valid request URL", 400)
		return
	}

	// go through each element and delete if url matches
	newFilters := config.Filters[:0]
	for _, filter := range config.Filters {
		if filter.URL != url {
			newFilters = append(newFilters, filter)
		}
	}
	config.Filters = newFilters
	err = writeAllConfigs()
	if err != nil {
		errortext := fmt.Sprintf("Couldn't write config file: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, http.StatusInternalServerError)
		return
	}
	err = writeFilterFile()
	if err != nil {
		errortext := fmt.Sprintf("Couldn't write filter file: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, http.StatusInternalServerError)
		return
	}
	tellCoreDNSToReload()
	fmt.Fprintf(w, "OK\n")
}

func handleFilteringEnableURL(w http.ResponseWriter, r *http.Request) {
	parameters, err := parseParametersFromBody(r.Body)
	if err != nil {
		errortext := fmt.Sprintf("failed to parse parameters from body: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, 400)
		return
	}

	url, ok := parameters["url"]
	if !ok {
		http.Error(w, "URL parameter was not specified", 400)
		return
	}

	if valid := govalidator.IsRequestURL(url); !valid {
		http.Error(w, "URL parameter is not valid request URL", http.StatusBadRequest)
		return
	}

	found := false
	for i := range config.Filters {
		filter := &config.Filters[i] // otherwise we will be operating on a copy
		if filter.URL == url {
			filter.Enabled = true
			found = true
		}
	}

	if !found {
		http.Error(w, "URL parameter was not previously added", http.StatusBadRequest)
		return
	}

	err = writeAllConfigs()
	if err != nil {
		errortext := fmt.Sprintf("Couldn't write config file: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, http.StatusInternalServerError)
		return
	}

	// kick off refresh of rules from new URLs
	refreshFiltersIfNeccessary()
	err = writeFilterFile()
	if err != nil {
		errortext := fmt.Sprintf("Couldn't write filter file: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, http.StatusInternalServerError)
		return
	}
	tellCoreDNSToReload()
	fmt.Fprintf(w, "OK\n")
}

func handleFilteringDisableURL(w http.ResponseWriter, r *http.Request) {
	parameters, err := parseParametersFromBody(r.Body)
	if err != nil {
		errortext := fmt.Sprintf("failed to parse parameters from body: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, 400)
		return
	}

	url, ok := parameters["url"]
	if !ok {
		http.Error(w, "URL parameter was not specified", 400)
		return
	}

	if valid := govalidator.IsRequestURL(url); !valid {
		http.Error(w, "URL parameter is not valid request URL", http.StatusBadRequest)
		return
	}

	found := false
	for i := range config.Filters {
		filter := &config.Filters[i] // otherwise we will be operating on a copy
		if filter.URL == url {
			filter.Enabled = false
			found = true
		}
	}

	if !found {
		http.Error(w, "URL parameter was not previously added", http.StatusBadRequest)
		return
	}

	err = writeAllConfigs()
	if err != nil {
		errortext := fmt.Sprintf("Couldn't write config file: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, http.StatusInternalServerError)
		return
	}
	err = writeFilterFile()
	if err != nil {
		errortext := fmt.Sprintf("Couldn't write filter file: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, http.StatusInternalServerError)
		return
	}
	tellCoreDNSToReload()
	fmt.Fprintf(w, "OK\n")
	// TODO: regenerate coredns config and tell coredns to reload it if it's running
}

func handleFilteringSetRules(w http.ResponseWriter, r *http.Request) {
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		errortext := fmt.Sprintf("Failed to read request body: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, 400)
		return
	}

	config.UserRules = strings.Split(string(body), "\n")
	err = writeAllConfigs()
	if err != nil {
		errortext := fmt.Sprintf("Couldn't write config file: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, http.StatusInternalServerError)
		return
	}
	err = writeFilterFile()
	if err != nil {
		errortext := fmt.Sprintf("Couldn't write filter file: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, http.StatusInternalServerError)
		return
	}
	tellCoreDNSToReload()
	fmt.Fprintf(w, "OK\n")
}

func handleFilteringRefresh(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force")
	if force != "" {
		config.Lock()
		for i := range config.Filters {
			filter := &config.Filters[i] // otherwise we will be operating on a copy
			filter.LastUpdated = time.Unix(0, 0)
		}
		config.Unlock() // not defer because refreshFiltersIfNeccessary locks it too
	}
	updated := refreshFiltersIfNeccessary()
	fmt.Fprintf(w, "OK %d filters updated\n", updated)
}

func runFilterRefreshers() {
	go func() {
		for range time.Tick(time.Second) {
			refreshFiltersIfNeccessary()
		}
	}()
}

func refreshFiltersIfNeccessary() int {
	now := time.Now()
	config.Lock()
	defer config.Unlock()

	// deduplicate
	// TODO: move it somewhere else
	{
		i := 0 // output index, used for deletion later
		urls := map[string]bool{}
		for _, filter := range config.Filters {
			if _, ok := urls[filter.URL]; !ok {
				// we didn't see it before, keep it
				urls[filter.URL] = true // remember the URL
				config.Filters[i] = filter
				i++
			}
		}
		// all entries we want to keep are at front, delete the rest
		config.Filters = config.Filters[:i]
	}

	// fetch URLs
	updateCount := 0
	for i := range config.Filters {
		filter := &config.Filters[i] // otherwise we will be operating on a copy
		updated, err := filter.update(now)
		if err != nil {
			log.Printf("Failed to update filter %s: %s\n", filter.URL, err)
			continue
		}
		if updated {
			updateCount++
		}
	}

	if updateCount > 0 {
		err := writeFilterFile()
		if err != nil {
			errortext := fmt.Sprintf("Couldn't write filter file: %s", err)
			log.Println(errortext)
		}
		tellCoreDNSToReload()
	}
	return updateCount
}

func (filter *filter) update(now time.Time) (bool, error) {
	if !filter.Enabled {
		return false, nil
	}
	elapsed := time.Since(filter.LastUpdated)
	if elapsed <= updatePeriod {
		return false, nil
	}

	// use same update period for failed filter downloads to avoid flooding with requests
	filter.LastUpdated = now

	log.Printf("Fetching URL %s...", filter.URL)
	resp, err := client.Get(filter.URL)
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		log.Printf("Couldn't request filter from URL %s, skipping: %s", filter.URL, err)
		return false, err
	}

	if resp.StatusCode >= 400 {
		log.Printf("Got status code %d from URL %s, skipping", resp.StatusCode, filter.URL)
		return false, fmt.Errorf("Got status code >= 400: %d", resp.StatusCode)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Couldn't fetch filter contents from URL %s, skipping: %s", filter.URL, err)
		return false, err
	}

	log.Printf("%s: got %v bytes", filter.URL, len(body))

	// extract filter name and count number of rules
	lines := strings.Split(string(body), "\n")
	rulesCount := 0
	seenTitle := false
	d := dnsfilter.New()
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line[0] == '!' {
			if m := filterTitle.FindAllStringSubmatch(line, -1); len(m) > 0 && len(m[0]) >= 2 && !seenTitle {
				log.Printf("Setting filter title to %s\n", m[0][1])
				filter.Name = m[0][1]
				seenTitle = true
			}
		} else if len(line) != 0 {
			err = d.AddRule(line, 0)
			if err == dnsfilter.ErrInvalidSyntax {
				continue
			}
			if err != nil {
				log.Printf("Couldn't add rule %s: %s", filter.URL, err)
				return false, err
			}
			rulesCount++
		}
	}
	if bytes.Equal(filter.contents, body) {
		log.Printf("Filter contents of URL %s are same, not considering it as an update", filter.URL)
		return false, nil
	}
	filter.RulesCount = rulesCount
	filter.contents = body
	return true, nil
}

// write filter file
func writeFilterFile() error {
	filterpath := filepath.Join(config.ourBinaryDir, config.CoreDNS.FilterFile)
	log.Printf("Writing filter file: %s", filterpath)
	// TODO: check if file contents have modified
	data := []byte{}
	filters := config.Filters
	for _, filter := range filters {
		if !filter.Enabled {
			continue
		}
		data = append(data, filter.contents...)
		data = append(data, '\n')
	}
	for _, rule := range config.UserRules {
		data = append(data, []byte(rule)...)
		data = append(data, '\n')
	}
	err := ioutil.WriteFile(filterpath+".tmp", data, 0644)
	if err != nil {
		log.Printf("Couldn't write filter file: %s", err)
		return err
	}

	err = os.Rename(filterpath+".tmp", filterpath)
	if err != nil {
		log.Printf("Couldn't rename filter file: %s", err)
		return err
	}
	return nil
}

// ------------
// safebrowsing
// ------------

func handleSafeBrowsingEnable(w http.ResponseWriter, r *http.Request) {
	config.CoreDNS.SafeBrowsingEnabled = true
	err := writeAllConfigsAndReloadCoreDNS()
	if err != nil {
		errortext := fmt.Sprintf("Couldn't write config file: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, http.StatusInternalServerError)
		return
	}
	fmt.Fprintf(w, "OK\n")
}

func handleSafeBrowsingDisable(w http.ResponseWriter, r *http.Request) {
	config.CoreDNS.SafeBrowsingEnabled = false
	err := writeAllConfigsAndReloadCoreDNS()
	if err != nil {
		errortext := fmt.Sprintf("Couldn't write config file: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, http.StatusInternalServerError)
		return
	}
	fmt.Fprintf(w, "OK\n")
}

func handleSafeBrowsingStatus(w http.ResponseWriter, r *http.Request) {
	data := map[string]interface{}{
		"enabled": config.CoreDNS.SafeBrowsingEnabled,
	}
	json, err := json.Marshal(data)
	if err != nil {
		errortext := fmt.Sprintf("Unable to marshal status json: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, 500)
	}

	w.Header().Set("Content-Type", "application/json")
	_, err = w.Write(json)
	if err != nil {
		errortext := fmt.Sprintf("Unable to write response json: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, 500)
		return
	}
}

// --------
// parental
// --------
func handleParentalEnable(w http.ResponseWriter, r *http.Request) {
	parameters, err := parseParametersFromBody(r.Body)
	if err != nil {
		errortext := fmt.Sprintf("failed to parse parameters from body: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, 400)
		return
	}

	sensitivity, ok := parameters["sensitivity"]
	if !ok {
		http.Error(w, "URL parameter was not specified", 400)
		return
	}

	switch sensitivity {
	case "3":
		break
	case "EARLY_CHILDHOOD":
		sensitivity = "3"
	case "10":
		break
	case "YOUNG":
		sensitivity = "10"
	case "13":
		break
	case "TEEN":
		sensitivity = "13"
	case "17":
		break
	case "MATURE":
		sensitivity = "17"
	default:
		http.Error(w, "Sensitivity must be set to valid value", 400)
		return
	}
	i, err := strconv.Atoi(sensitivity)
	if err != nil {
		http.Error(w, "Sensitivity must be set to valid value", 400)
		return
	}
	config.CoreDNS.ParentalSensitivity = i
	config.CoreDNS.ParentalEnabled = true
	err = writeAllConfigsAndReloadCoreDNS()
	if err != nil {
		errortext := fmt.Sprintf("Couldn't write config file: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, http.StatusInternalServerError)
		return
	}
	fmt.Fprintf(w, "OK\n")
}

func handleParentalDisable(w http.ResponseWriter, r *http.Request) {
	config.CoreDNS.ParentalEnabled = false
	err := writeAllConfigsAndReloadCoreDNS()
	if err != nil {
		errortext := fmt.Sprintf("Couldn't write config file: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, http.StatusInternalServerError)
		return
	}
	fmt.Fprintf(w, "OK\n")
}

func handleParentalStatus(w http.ResponseWriter, r *http.Request) {
	data := map[string]interface{}{
		"enabled": config.CoreDNS.ParentalEnabled,
	}
	if config.CoreDNS.ParentalEnabled {
		data["sensitivity"] = config.CoreDNS.ParentalSensitivity
	}
	json, err := json.Marshal(data)
	if err != nil {
		errortext := fmt.Sprintf("Unable to marshal status json: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, 500)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, err = w.Write(json)
	if err != nil {
		errortext := fmt.Sprintf("Unable to write response json: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, 500)
		return
	}
}

// ------------
// safebrowsing
// ------------

func handleSafeSearchEnable(w http.ResponseWriter, r *http.Request) {
	config.CoreDNS.SafeSearchEnabled = true
	err := writeAllConfigsAndReloadCoreDNS()
	if err != nil {
		errortext := fmt.Sprintf("Couldn't write config file: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, http.StatusInternalServerError)
		return
	}
	fmt.Fprintf(w, "OK\n")
}

func handleSafeSearchDisable(w http.ResponseWriter, r *http.Request) {
	config.CoreDNS.SafeSearchEnabled = false
	err := writeAllConfigsAndReloadCoreDNS()
	if err != nil {
		errortext := fmt.Sprintf("Couldn't write config file: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, http.StatusInternalServerError)
		return
	}
	fmt.Fprintf(w, "OK\n")
}

func handleSafeSearchStatus(w http.ResponseWriter, r *http.Request) {
	data := map[string]interface{}{
		"enabled": config.CoreDNS.SafeSearchEnabled,
	}
	json, err := json.Marshal(data)
	if err != nil {
		errortext := fmt.Sprintf("Unable to marshal status json: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, 500)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, err = w.Write(json)
	if err != nil {
		errortext := fmt.Sprintf("Unable to write response json: %s", err)
		log.Println(errortext)
		http.Error(w, errortext, 500)
		return
	}
}

func registerControlHandlers() {
	http.HandleFunc("/control/start", ensurePOST(handleStart))
	http.HandleFunc("/control/stop", ensurePOST(handleStop))
	http.HandleFunc("/control/restart", ensurePOST(handleRestart))
	http.HandleFunc("/control/status", ensureGET(handleStatus))
	http.HandleFunc("/control/stats", ensureGET(handleStats))
	http.HandleFunc("/control/stats_history", ensureGET(handleStatsHistory))
	http.HandleFunc("/control/stats_top", ensureGET(handleStatsTop))
	http.HandleFunc("/control/querylog", handleQueryLog)
	http.HandleFunc("/control/querylog_enable", ensurePOST(handleQueryLogEnable))
	http.HandleFunc("/control/querylog_disable", ensurePOST(handleQueryLogDisable))
	http.HandleFunc("/control/set_upstream_dns", ensurePOST(handleSetUpstreamDNS))
	http.HandleFunc("/control/filtering/enable", ensurePOST(handleFilteringEnable))
	http.HandleFunc("/control/filtering/disable", ensurePOST(handleFilteringDisable))
	http.HandleFunc("/control/filtering/status", ensureGET(handleFilteringStatus))
	http.HandleFunc("/control/filtering/add_url", ensurePUT(handleFilteringAddURL))
	http.HandleFunc("/control/filtering/remove_url", ensureDELETE(handleFilteringRemoveURL))
	http.HandleFunc("/control/filtering/enable_url", ensurePOST(handleFilteringEnableURL))
	http.HandleFunc("/control/filtering/disable_url", ensurePOST(handleFilteringDisableURL))
	http.HandleFunc("/control/filtering/set_rules", ensurePUT(handleFilteringSetRules))
	http.HandleFunc("/control/filtering/refresh", ensurePOST(handleFilteringRefresh))
	http.HandleFunc("/control/safebrowsing/enable", ensurePOST(handleSafeBrowsingEnable))
	http.HandleFunc("/control/safebrowsing/disable", ensurePOST(handleSafeBrowsingDisable))
	http.HandleFunc("/control/safebrowsing/status", ensureGET(handleSafeBrowsingStatus))
	http.HandleFunc("/control/parental/enable", ensurePOST(handleParentalEnable))
	http.HandleFunc("/control/parental/disable", ensurePOST(handleParentalDisable))
	http.HandleFunc("/control/parental/status", ensureGET(handleParentalStatus))
	http.HandleFunc("/control/safesearch/enable", ensurePOST(handleSafeSearchEnable))
	http.HandleFunc("/control/safesearch/disable", ensurePOST(handleSafeSearchDisable))
	http.HandleFunc("/control/safesearch/status", ensureGET(handleSafeSearchStatus))
}
