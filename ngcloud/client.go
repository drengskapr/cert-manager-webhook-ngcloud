package ngcloud

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	defaultBaseURL   = "https://lk-api-gateway.ngcloud.ru/api/v1/svc"
	serviceID        = 111
	svcOpCreate      = 45
	svcOpDelete      = 46
	pollMaxAttempts  = 120
	pollInterval     = 5 * time.Second
	operationTimeout = 120 * time.Second
	defaultTTL       = "120"
)

// CFS parameter labels
const (
	cfsLabelZoneUID     = "UUID \u0417\u043e\u043d\u044b"
	cfsLabelRecordType  = "\u0422\u0438\u043f DNS-\u0437\u0430\u043f\u0438\u0441\u0438"
	cfsLabelRecordName  = "\u0418\u043c\u044f \u0437\u0430\u043f\u0438\u0441\u0438"
	cfsLabelRecordValue = "\u0417\u043d\u0430\u0447\u0435\u043d\u0438\u0435 \u0437\u0430\u043f\u0438\u0441\u0438"
	cfsLabelTTL         = "TTL \u0437\u0430\u043f\u0438\u0441\u0438 (\u0432 \u0441\u0435\u043a\u0443\u043d\u0434\u0430\u0445)"
)

var uuidRe = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// state of Instance
type instancesResponse struct {
	Results []struct {
		InstanceUID string `json:"instanceUid"`
		DisplayName string `json:"displayName"`
		DtCreated   string `json:"instanceConfigDtCreated"`
	} `json:"results"`
}

// state of Instance Op (save for cache)
type operationState struct {
	mu        sync.Mutex
	opUID     string
	status    string // "pending", "running", "success", "failed"
	err       error
	startTime time.Time
	done      chan struct{}
}

// state of Cfs
type cfsResponse struct {
	SvcOperation struct {
		CfsParams []struct {
			Label                  string `json:"label"`
			SvcOperationCfsParamID int    `json:"svcOperationCfsParamId"`
		} `json:"cfsParams"`
	} `json:"svcOperation"`
}

// state of Instance op (response from deck for check is job done)
type operationStatusResponse struct {
	InstanceOperation struct {
		InstanceOperationUid string  `json:"instanceOperationUid"`
		Operation            string  `json:"operation"`
		InstanceUid          string  `json:"instanceUid"`
		DtSubmit             string  `json:"dtSubmit"`
		SubmitResult         string  `json:"submitResult"`
		ErrorLog             string  `json:"errorLog"`
		DtStart              string  `json:"dtStart"`
		DtFinish             string  `json:"dtFinish"`
		Duration             float64 `json:"duration"`
		IsSuccessful         bool    `json:"isSuccessful"`
		DisplayName          string  `json:"displayName"`
		DtCreated            string  `json:"dtCreated"`
		DtUpdated            string  `json:"dtUpdated"`
		IsInProgress         bool    `json:"isInProgress"`
		IsPending            bool    `json:"isPending"`
	} `json:"instanceOperation"`
}

// HTTP client for the api deck
type Client struct {
	baseURL      string
	token        string
	httpClient   *http.Client
	log          logr.Logger
	operations   map[string]*operationState // cache
	operationsMu sync.RWMutex               // access granted
}

// New creates a Client using the provided Bearer token.
func New(token string) *Client {
	return &Client{
		baseURL:    defaultBaseURL,
		token:      token,
		httpClient: &http.Client{Timeout: 120 * time.Second},
		log:        ctrl.Log.WithName("ngcloud-client"),
		operations: make(map[string]*operationState),
	}
}

//* DNS *//

// CreateTXTRecord creates a DNS TXT record
func (c *Client) CreateTXTRecord(zoneUID, recordName, recordValue string) error {
	log := c.log.WithValues("recordName", recordName, "zoneUID", zoneUID)
	log.V(1).Info("CreateTXTRecord called")

	displayName := "dnsrecord-" + recordName
	opKey := displayName

	// Check if op is already started
	c.operationsMu.RLock()
	state, exists := c.operations[opKey]
	c.operationsMu.RUnlock()

	if exists {
		state.mu.Lock()
		status := state.status
		state.mu.Unlock()

		if status == "running" || status == "pending" {
			log.V(1).Info("Operation already in progress, waiting", "status", status)
			<-state.done
			if state.err != nil {
				return state.err
			}
			return nil
		}
		// if op completed, lock and close the op
		c.operationsMu.Lock()
		delete(c.operations, opKey)
		c.operationsMu.Unlock()
	}

	// Create op state
	state = &operationState{
		status:    "pending",
		startTime: time.Now(),
		done:      make(chan struct{}),
	}
	c.operationsMu.Lock()
	c.operations[opKey] = state
	c.operationsMu.Unlock()

	// Start op in background with create parameters
	go func() {
		defer close(state.done)
		state.mu.Lock()
		state.status = "running"
		state.mu.Unlock()

		log.V(1).Info("Starting background execution")

		// Execute create operation
		if err := c.executeOperation(displayName, "create", state, func(opUID string) error {
			// Fetch CFS parameters
			cfsIDs, err := c.fetchCFSParams(svcOpCreate)
			if err != nil {
				return fmt.Errorf("fetch CFS params: %w", err)
			}

			// Push CFS parameters
			params := map[string]string{
				cfsLabelZoneUID:     zoneUID,
				cfsLabelRecordType:  "TXT",
				cfsLabelRecordName:  recordName,
				cfsLabelRecordValue: recordValue,
				cfsLabelTTL:         defaultTTL,
			}
			return c.pushAllCFSParams(opUID, cfsIDs, params)
		}); err != nil {
			state.mu.Lock()
			state.status = "failed"
			state.err = err
			state.mu.Unlock()
			log.Error(err, "Background execution failed")
			return
		}

		state.mu.Lock()
		state.status = "success"
		state.mu.Unlock()
		log.V(1).Info("Background execution completed successfully")
	}()

	// Wait for complete op
	select {
	case <-state.done:
		if state.err != nil {
			return state.err
		}
		return nil
	case <-time.After(5 * time.Second):
		/*
			Workaround for when the deck-api hangs.
			In principle this branch should not be reached, but if all workers are
			in a cold start and stall on startup (registry access, new image, etc.),
			it prevents the operation from being re-triggered. Feels like a small delay.
		*/
		log.V(1).Info("Operation started in background, returning success")
		return nil
	}
}

// DeleteTXTRecord deletes the DNS record instance.
func (c *Client) DeleteTXTRecord(recordName string) error {
	log := c.log.WithValues("recordName", recordName)
	log.V(1).Info("DeleteTXTRecord called")

	displayName := "dnsrecord-" + recordName

	// Execute delete operation (no pushParams needed)
	err := c.executeOperation(displayName, "delete", nil, nil)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			log.V(1).Info("Instance not found, assuming already deleted")
			return nil
		}
		return err
	}

	log.V(1).Info("DeleteTXTRecord completed successfully")
	return nil
}

//* Instance *//

func (c *Client) createInstance(displayName string) error {
	log := c.log.WithValues("displayName", displayName)

	body := map[string]interface{}{
		"serviceId":   serviceID,
		"displayName": displayName,
		"descr":       "",
	}
	log.V(1).Info("Creating instance", "body", body)

	startTime := time.Now()
	resp, err := c.post(c.baseURL+"/instances", body)
	if err != nil {
		log.Error(err, "HTTP POST failed")
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	log.V(1).Info("Create instance response", "statusCode", resp.StatusCode, "duration", time.Since(startTime))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusBadRequest && bytes.Contains(respBody, []byte("not unique")) {
			log.V(1).Info("Instance already exists (not unique)")
			return nil
		}
		log.Error(nil, "Create instance failed", "statusCode", resp.StatusCode, "body", string(respBody))
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, respBody)
	}

	log.V(1).Info("Instance created successfully")
	return nil
}

func (c *Client) getInstanceUID(displayName string) (string, error) {
	log := c.log.WithValues("displayName", displayName)
	url := fmt.Sprintf(
		"%s/instances?fields=instanceUid,displayName,instanceConfigDtCreated&page=1&pageSize=100&serviceId=%d",
		c.baseURL, serviceID,
	)
	log.V(1).Info("Getting instance UID", "url", url)

	startTime := time.Now()
	resp, err := c.get(url)
	if err != nil {
		log.Error(err, "HTTP GET failed")
		return "", err
	}
	defer resp.Body.Close()

	log.V(1).Info("Get instances response", "statusCode", resp.StatusCode, "duration", time.Since(startTime))

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	var result instancesResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Error(err, "Failed to decode response")
		return "", err
	}

	// Get oldest instance (most likely the active one)
	oldest := ""
	oldestTime := ""
	for _, inst := range result.Results {
		if inst.DisplayName != displayName {
			continue
		}
		if oldest == "" || inst.DtCreated < oldestTime {
			oldest = inst.InstanceUID
			oldestTime = inst.DtCreated
		}
	}

	if oldest == "" {
		log.V(1).Info("Instance not found")
		return "", fmt.Errorf("instance with displayName %q not found", displayName)
	}

	log.V(1).Info("Found instance", "instanceUID", oldest, "createdAt", oldestTime)
	return oldest, nil
}

// Above creating 100500 eq instances
func (c *Client) getOrCreateInstance(displayName string) (string, error) {
	log := c.log.WithValues("displayName", displayName)

	uid, err := c.getInstanceUID(displayName)
	if err == nil && uid != "" {
		log.V(1).Info("Found existing instance", "instanceUID", uid)
		return uid, nil
	}

	log.V(1).Info("Creating new instance")
	if err := c.createInstance(displayName); err != nil {
		return "", err
	}

	uid, err = c.getInstanceUID(displayName)
	if err != nil {
		return "", err
	}

	log.V(1).Info("Created new instance", "instanceUID", uid)
	return uid, nil
}

//* Instance *//

func (c *Client) createOperation(instanceUID, operation string) (string, error) {
	log := c.log.WithValues("instanceUID", instanceUID, "operation", operation)
	body := map[string]string{
		"instanceUid": instanceUID,
		"operation":   operation,
	}
	log.V(1).Info("Creating operation", "body", body)

	startTime := time.Now()
	resp, err := c.post(c.baseURL+"/instanceOperations", body)
	if err != nil {
		log.Error(err, "HTTP POST failed")
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	log.V(1).Info("Create operation response", "statusCode", resp.StatusCode, "duration", time.Since(startTime))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == 409 || resp.StatusCode == 400 {
			location := resp.Header.Get("Location")
			if uid := uuidRe.FindString(location); uid != "" {
				log.V(1).Info("Operation already exists, reusing", "operationUID", uid)
				return uid, nil
			}
		}
		log.Error(nil, "Create operation failed", "statusCode", resp.StatusCode, "body", string(respBody))
		return "", fmt.Errorf("unexpected status %d: %s", resp.StatusCode, respBody)
	}

	location := resp.Header.Get("Location")
	uid := uuidRe.FindString(location)
	if uid == "" {
		log.Error(nil, "Failed to extract UID from Location header", "location", location)
		return "", fmt.Errorf("could not extract operation UID from Location header: %q", location)
	}

	log.V(1).Info("Operation created", "operationUID", uid)
	return uid, nil
}

// executeOperation is the core function that handles both create and delete operations
// For create operations, pushParams must be provided
// For delete operations, pushParams can be nil
// TODO: if need to use on another jobs, may be used as mainfunc (op modify / reconcile / etc)
func (c *Client) executeOperation(displayName, opType string, state *operationState, pushParams func(string) error) error {
	log := c.log.WithValues("displayName", displayName, "operation", opType)

	// Get instance UID
	var instanceUID string
	var err error

	if opType == "create" {
		// For create: get or create instance
		instanceUID, err = c.getOrCreateInstance(displayName)
	} else {
		// For delete: get existing instance only
		instanceUID, err = c.getInstanceUID(displayName)
	}

	if err != nil {
		return fmt.Errorf("get instance: %w", err)
	}
	log.V(1).Info("Instance ready", "instanceUID", instanceUID)

	// Create operation
	opUID, err := c.createOperation(instanceUID, opType)
	if err != nil {
		return fmt.Errorf("create operation: %w", err)
	}
	log.V(1).Info("Operation created", "operationUID", opUID)

	if state != nil {
		state.mu.Lock()
		state.opUID = opUID
		state.mu.Unlock()
	}

	// Push parameters (create op)
	if opType == "create" && pushParams != nil {
		if err := pushParams(opUID); err != nil {
			return fmt.Errorf("push params: %w", err)
		}
	}

	// Run operation
	if err := c.runOperation(opUID); err != nil {
		return fmt.Errorf("run operation: %w", err)
	}

	// Wait for operation
	if err := c.waitForOperation(opUID); err != nil {
		return fmt.Errorf("wait operation: %w", err)
	}

	return nil
}

func (c *Client) runOperation(opUID string) error {
	log := c.log.WithValues("operationUID", opUID)
	url := fmt.Sprintf("%s/instanceOperations/%s/run", c.baseURL, opUID)
	log.V(1).Info("Running operation", "url", url)

	startTime := time.Now()
	resp, err := c.post(url, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	log.V(1).Info("Run operation response", "statusCode", resp.StatusCode, "duration", time.Since(startTime), "body", string(body))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == 422 && strings.Contains(string(body), "Concurrent operations") {
			log.V(1).Info("Operation already running, will wait for completion")
			return nil
		}
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

func (c *Client) waitForOperation(opUID string) error {
	log := c.log.WithValues("operationUID", opUID)
	log.V(1).Info("Waiting for operation to complete", "maxAttempts", pollMaxAttempts, "interval", pollInterval)

	url := fmt.Sprintf("%s/instanceOperations/%s", c.baseURL, opUID)
	startTime := time.Now()

	for attempt := 0; attempt < pollMaxAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(pollInterval)
		}

		resp, err := c.get(url)
		if err != nil {
			log.Error(err, "Poll request failed", "attempt", attempt)
			return err
		}

		var status operationStatusResponse
		if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
			resp.Body.Close()
			log.Error(err, "Failed to decode poll response", "attempt", attempt)
			return fmt.Errorf("decode status: %w", err)
		}
		resp.Body.Close()

		op := status.InstanceOperation

		if op.DtCreated == "" {
			log.V(1).Info("Operation not started yet", "attempt", attempt)
			continue
		}

		if op.SubmitResult != "" && op.SubmitResult != "201" && op.SubmitResult != "200" {
			log.Error(nil, "Operation submission failed", "submitResult", op.SubmitResult, "errorLog", op.ErrorLog)
			return fmt.Errorf("operation submission failed: %s", op.ErrorLog)
		}

		log.V(1).Info("Poll response", "attempt", attempt, "duration", time.Since(startTime),
			"isSuccessful", op.IsSuccessful, "dtFinish", op.DtFinish, "submitResult", op.SubmitResult)

		if op.DtFinish != "" {
			if op.IsSuccessful {
				log.V(1).Info("Operation completed successfully", "attempts", attempt+1, "duration", time.Since(startTime))
				return nil
			}
			if strings.Contains(op.ErrorLog, "\u0423\u0441\u043b\u0443\u0433\u0430 \u0443\u0434\u0430\u043b\u0435\u043d\u0430") ||
				strings.Contains(op.ErrorLog, "already deleted") ||
				strings.Contains(op.ErrorLog, "not found") {
				log.V(1).Info("Operation completed with service deleted, treating as success")
				return nil
			}
			log.Error(nil, "Operation failed", "errorLog", op.ErrorLog)
			return fmt.Errorf("operation failed: %s", op.ErrorLog)
		}

		if time.Since(startTime) > operationTimeout {
			log.Error(nil, "Operation timed out", "elapsed", time.Since(startTime))
			return fmt.Errorf("operation timed out after %v", operationTimeout)
		}
	}

	log.Error(nil, "Operation timed out", "attempts", pollMaxAttempts)
	return fmt.Errorf("operation timed out after %d attempts", pollMaxAttempts)
}

//* CFS *//

func (c *Client) fetchCFSParams(svcOpID int) (map[string]int, error) {
	log := c.log.WithValues("svcOpID", svcOpID)
	url := fmt.Sprintf(
		"%s/instanceOperations/default/%d?fields=operation,svcOperationId,cfsParams",
		c.baseURL, svcOpID,
	)
	log.V(1).Info("Fetching CFS parameters", "url", url)

	startTime := time.Now()
	resp, err := c.get(url)
	if err != nil {
		log.Error(err, "HTTP GET failed", "url", url)
		return nil, err
	}
	defer resp.Body.Close()

	log.V(1).Info("CFS params response", "statusCode", resp.StatusCode, "duration", time.Since(startTime))

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Error(nil, "Unexpected status code", "statusCode", resp.StatusCode, "body", string(body))
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	var result cfsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Error(err, "Failed to decode response")
		return nil, err
	}

	m := make(map[string]int, len(result.SvcOperation.CfsParams))
	for _, p := range result.SvcOperation.CfsParams {
		m[p.Label] = p.SvcOperationCfsParamID
	}

	log.V(1).Info("CFS parameters fetched", "count", len(m))
	return m, nil
}

func (c *Client) pushAllCFSParams(opUID string, cfsIDs map[string]int, params map[string]string) error {
	log := c.log.WithValues("operationUID", opUID)

	for label, value := range params {
		id, ok := cfsIDs[label]
		if !ok {
			log.V(1).Info("CFS parameter label not found, skipping", "label", label)
			continue
		}
		log.V(1).Info("Pushing CFS param", "label", label, "value", value, "paramID", id)
		if err := c.pushCFSParam(opUID, id, value); err != nil {
			return fmt.Errorf("push CFS param %q: %w", label, err)
		}
	}
	return nil
}

func (c *Client) pushCFSParam(operationUID string, paramID int, value string) error {
	log := c.log.WithValues("operationUID", operationUID, "paramID", paramID, "value", value)
	body := map[string]interface{}{
		"paramValue":             value,
		"instanceOperationUid":   operationUID,
		"svcOperationCfsParamId": paramID,
	}
	log.V(1).Info("Pushing CFS param", "body", body)

	startTime := time.Now()
	resp, err := c.post(c.baseURL+"/instanceOperationCfsParams", body)
	if err != nil {
		log.Error(err, "HTTP POST failed")
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	log.V(1).Info("Push CFS param response", "statusCode", resp.StatusCode, "duration", time.Since(startTime))

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		log.Error(nil, "Unexpected status", "statusCode", resp.StatusCode, "body", string(respBody))
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	return nil
}

//* HTTP Helpers *//

func (c *Client) get(url string) (*http.Response, error) {
	log := c.log.WithValues("method", "GET", "url", url)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		log.Error(err, "Failed to create request")
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)

	log.V(1).Info("Sending HTTP request")
	return c.httpClient.Do(req)
}

func (c *Client) post(url string, body interface{}) (*http.Response, error) {
	log := c.log.WithValues("method", "POST", "url", url)
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			log.Error(err, "Failed to marshal request body")
			return nil, err
		}
		log.V(1).Info("Request body", "body", string(b))
		r = bytes.NewReader(b)
	}

	req, err := http.NewRequest(http.MethodPost, url, r)
	if err != nil {
		log.Error(err, "Failed to create request")
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	log.V(1).Info("Sending HTTP request")
	return c.httpClient.Do(req)
}
