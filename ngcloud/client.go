package ngcloud

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	defaultBaseURL  = "https://deck-api.ngcloud.ru/api/v1/index.cfm"
	serviceID       = 111
	svcOpCreate     = 45
	svcOpDelete     = 46
	pollMaxAttempts = 60
	pollInterval    = 5 * time.Second
	defaultTTL      = "120"
)

// CFS parameter labels as returned by the deck-api. These are API-defined
// strings and must match exactly; they cannot be translated.
const (
	cfsLabelZoneUID     = "UUID \u0417\u043e\u043d\u044b"
	cfsLabelRecordType  = "\u0422\u0438\u043f DNS-\u0437\u0430\u043f\u0438\u0441\u0438"
	cfsLabelRecordName  = "\u0418\u043c\u044f \u0437\u0430\u043f\u0438\u0441\u0438"
	cfsLabelRecordValue = "\u0417\u043d\u0430\u0447\u0435\u043d\u0438\u0435 \u0437\u0430\u043f\u0438\u0441\u0438"
	cfsLabelTTL         = "TTL \u0437\u0430\u043f\u0438\u0441\u0438 (\u0432 \u0441\u0435\u043a\u0443\u043d\u0434\u0430\u0445)"
)

var uuidRe = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// Client is an HTTP client for the Ngcloud deck-api.
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

// New creates a Client using the provided Bearer token.
func New(token string) *Client {
	return &Client{
		baseURL:    defaultBaseURL,
		token:      token,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// CreateTXTRecord creates a DNS TXT record via the deck-api (svcOperationId=45).
// It follows the full 6-step flow: fetch CFS params → create instance → create
// operation → push CFS params → run → poll.
func (c *Client) CreateTXTRecord(zoneUID, recordName, recordValue string) error {
	cfsIDs, err := c.fetchCFSParams(svcOpCreate)
	if err != nil {
		return fmt.Errorf("fetch CFS params: %w", err)
	}

	displayName := "dnsrecord-" + recordName
	if err := c.createInstance(displayName); err != nil {
		return fmt.Errorf("create instance: %w", err)
	}

	instanceUID, err := c.getInstanceUID(displayName)
	if err != nil {
		return fmt.Errorf("get instance UID: %w", err)
	}

	opUID, err := c.createOperation(instanceUID, "create")
	if err != nil {
		return fmt.Errorf("create operation: %w", err)
	}

	params := map[string]string{
		cfsLabelZoneUID:     zoneUID,
		cfsLabelRecordType:  "TXT",
		cfsLabelRecordName:  recordName,
		cfsLabelRecordValue: recordValue,
		cfsLabelTTL:         defaultTTL,
	}
	for label, value := range params {
		id, ok := cfsIDs[label]
		if !ok {
			continue
		}
		if err := c.pushCFSParam(opUID, id, value); err != nil {
			return fmt.Errorf("push CFS param %q: %w", label, err)
		}
	}

	pollUID, err := c.runOperation(opUID)
	if err != nil {
		return fmt.Errorf("run operation: %w", err)
	}
	return c.pollOperation(pollUID)
}

// DeleteTXTRecord deletes the DNS record instance via the deck-api (svcOperationId=46).
// It locates the existing instance by record name, runs a delete operation, and polls
// for completion. CFS params are not required for delete.
func (c *Client) DeleteTXTRecord(recordName string) error {
	displayName := "dnsrecord-" + recordName

	instanceUID, err := c.getInstanceUID(displayName)
	if err != nil {
		return fmt.Errorf("get instance UID: %w", err)
	}

	opUID, err := c.createOperation(instanceUID, "delete")
	if err != nil {
		return fmt.Errorf("create operation: %w", err)
	}

	pollUID, err := c.runOperation(opUID)
	if err != nil {
		return fmt.Errorf("run operation: %w", err)
	}
	return c.pollOperation(pollUID)
}

// --- internal helpers ---

type cfsResponse struct {
	SvcOperation struct {
		CfsParams []struct {
			Label                  string `json:"label"`
			SvcOperationCfsParamID int    `json:"svcOperationCfsParamId"`
		} `json:"cfsParams"`
	} `json:"svcOperation"`
}

func (c *Client) fetchCFSParams(svcOpID int) (map[string]int, error) {
	url := fmt.Sprintf(
		"%s/instanceOperations/default/%d?fields=operation,svcOperationId,cfsParams",
		c.baseURL, svcOpID,
	)
	resp, err := c.get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	var result cfsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	m := make(map[string]int, len(result.SvcOperation.CfsParams))
	for _, p := range result.SvcOperation.CfsParams {
		m[p.Label] = p.SvcOperationCfsParamID
	}
	return m, nil
}

func (c *Client) createInstance(displayName string) error {
	body := map[string]interface{}{
		"serviceId":   serviceID,
		"displayName": displayName,
		"descr":       "",
	}
	resp, err := c.post(c.baseURL+"/instances", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 400 "not unique" means the instance already exists; treat as success so
		// the caller proceeds to getInstanceUID and reuses the existing instance.
		if resp.StatusCode == http.StatusBadRequest && bytes.Contains(respBody, []byte("not unique")) {
			return nil
		}
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, respBody)
	}
	return nil
}

type instancesResponse struct {
	Results []struct {
		InstanceUID string `json:"instanceUid"`
		DisplayName string `json:"displayName"`
		DtCreated   string `json:"instanceConfigDtCreated"`
	} `json:"results"`
}

// getInstanceUID returns the UID of the most recently created instance with
// the given displayName. Multiple instances with the same name can exist when
// a previous operation left a stale helper; the newest one is the live record.
func (c *Client) getInstanceUID(displayName string) (string, error) {
	url := fmt.Sprintf(
		"%s/instances?fields=instanceUid,displayName,instanceConfigDtCreated&page=1&pageSize=100&serviceId=%d",
		c.baseURL, serviceID,
	)
	resp, err := c.get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	var result instancesResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	latest := ""
	latestTime := ""
	for _, inst := range result.Results {
		if inst.DisplayName != displayName {
			continue
		}
		if latest == "" || inst.DtCreated > latestTime {
			latest = inst.InstanceUID
			latestTime = inst.DtCreated
		}
	}
	if latest == "" {
		return "", fmt.Errorf("instance with displayName %q not found", displayName)
	}
	return latest, nil
}

func (c *Client) createOperation(instanceUID, operation string) (string, error) {
	body := map[string]string{
		"instanceUid": instanceUID,
		"operation":   operation,
	}
	resp, err := c.post(c.baseURL+"/instanceOperations", body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body) //nolint:errcheck

	location := resp.Header.Get("Location")
	uid := uuidRe.FindString(location)
	if uid == "" {
		return "", fmt.Errorf("could not extract operation UID from Location header: %q", location)
	}
	return uid, nil
}

func (c *Client) pushCFSParam(operationUID string, paramID int, value string) error {
	body := map[string]interface{}{
		"paramValue":             value,
		"instanceOperationUid":   operationUID,
		"svcOperationCfsParamId": paramID,
	}
	resp, err := c.post(c.baseURL+"/instanceOperationCfsParams", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body) //nolint:errcheck

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return nil
}

// runOperation submits the operation for execution. It returns the UID to poll:
// if the /run response itself contains a Location header with a new UUID (the
// platform can create a child operation), that child UID is returned; otherwise
// the original operationUID is returned.
func (c *Client) runOperation(operationUID string) (string, error) {
	url := fmt.Sprintf("%s/instanceOperations/%s/run", c.baseURL, operationUID)
	resp, err := c.post(url, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body) //nolint:errcheck

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	if loc := resp.Header.Get("Location"); loc != "" {
		if uid := uuidRe.FindString(loc); uid != "" && uid != operationUID {
			return uid, nil
		}
	}
	return operationUID, nil
}

type operationStatusResponse struct {
	InstanceOperation struct {
		IsSuccessful bool   `json:"isSuccessful"`
		DtFinish     string `json:"dtFinish"`
		ErrorLog     string `json:"errorLog"`
	} `json:"instanceOperation"`
}

func (c *Client) pollOperation(operationUID string) error {
	url := fmt.Sprintf("%s/instanceOperations/%s", c.baseURL, operationUID)
	for attempt := 0; attempt < pollMaxAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(pollInterval)
		}

		resp, err := c.get(url)
		if err != nil {
			return err
		}
		var status operationStatusResponse
		err = json.NewDecoder(resp.Body).Decode(&status)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("decode status: %w", err)
		}

		op := status.InstanceOperation
		if op.IsSuccessful {
			return nil
		}
		if op.DtFinish != "" {
			// "Услуга удалена" (service deleted) is a platform confirmation that
			// the DNS record instance was removed; treat it as success.
			if strings.Contains(op.ErrorLog, "\u0423\u0441\u043b\u0443\u0433\u0430 \u0443\u0434\u0430\u043b\u0435\u043d\u0430") {
				return nil
			}
			return fmt.Errorf("operation failed: %s", op.ErrorLog)
		}
	}
	return fmt.Errorf("operation timed out after %d attempts", pollMaxAttempts)
}

// --- HTTP helpers ---

func (c *Client) get(url string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	return c.httpClient.Do(req)
}

func (c *Client) post(url string, body interface{}) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(http.MethodPost, url, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.httpClient.Do(req)
}
