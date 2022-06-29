package posthog

import (
	"bytes"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

const LONG_SCALE = 0xfffffffffffffff

type FeatureFlagsPoller struct {
	ticker                       *time.Ticker // periodic ticker
	loaded                       chan bool
	shutdown                     chan bool
	forceReload                  chan bool
	featureFlags                 []FeatureFlag
	personalApiKey               string
	projectApiKey                string
	Errorf                       func(format string, args ...interface{})
	Endpoint                     string
	http                         http.Client
	mutex                        sync.RWMutex
	fetchedFlagsSuccessfullyOnce bool
}

type FeatureFlag struct {
	Key               string `json:"key"`
	IsSimpleFlag      bool   `json:"is_simple_flag"`
	RolloutPercentage *uint8 `json:"rollout_percentage"`
	Active            bool   `json:"active"`
}

type FeatureFlagsResponse struct {
	Results []FeatureFlag `json:"results"`
}

type DecideRequestData struct {
	ApiKey     string `json:"api_key"`
	DistinctId string `json:"distinct_id"`
}

type DecideResponse struct {
	FeatureFlags map[string]interface{} `json:"featureFlags"`
}

func newFeatureFlagsPoller(projectApiKey string, personalApiKey string, errorf func(format string, args ...interface{}), endpoint string, httpClient http.Client, pollingInterval time.Duration) *FeatureFlagsPoller {
	poller := FeatureFlagsPoller{
		ticker:                       time.NewTicker(pollingInterval),
		loaded:                       make(chan bool),
		shutdown:                     make(chan bool),
		forceReload:                  make(chan bool),
		personalApiKey:               personalApiKey,
		projectApiKey:                projectApiKey,
		Errorf:                       errorf,
		Endpoint:                     endpoint,
		http:                         httpClient,
		mutex:                        sync.RWMutex{},
		fetchedFlagsSuccessfullyOnce: false,
	}

	go poller.run()
	return &poller
}

func (poller *FeatureFlagsPoller) run() {
	poller.fetchNewFeatureFlags()

	for {
		select {
		case <-poller.shutdown:
			close(poller.shutdown)
			close(poller.forceReload)
			close(poller.loaded)
			poller.ticker.Stop()
			return
		case <-poller.forceReload:
			poller.fetchNewFeatureFlags()
		case <-poller.ticker.C:
			poller.fetchNewFeatureFlags()
		}
	}
}

func (poller *FeatureFlagsPoller) fetchNewFeatureFlags() {
	personalApiKey := poller.personalApiKey
	requestData := []byte{}
	headers := [][2]string{{"Authorization", "Bearer " + personalApiKey + ""}}
	res, err := poller.request("GET", "api/feature_flag", requestData, headers)
	if err != nil || res.StatusCode != http.StatusOK {
		poller.Errorf("Unable to fetch feature flags", err)
	}
	defer res.Body.Close()
	resBody, err := ioutil.ReadAll(res.Body)
	if err != nil {
		poller.Errorf("Unable to fetch feature flags", err)
		return
	}
	featureFlagsResponse := FeatureFlagsResponse{}
	err = json.Unmarshal([]byte(resBody), &featureFlagsResponse)
	if err != nil {
		poller.Errorf("Unable to unmarshal response from api/feature_flag", err)
		return
	}
	if !poller.fetchedFlagsSuccessfullyOnce {
		poller.loaded <- true
	}
	newFlags := []FeatureFlag{}
	for _, flag := range featureFlagsResponse.Results {
		if flag.Active {
			newFlags = append(newFlags, flag)
		}
	}
	poller.mutex.Lock()
	poller.featureFlags = newFlags
	poller.mutex.Unlock()

}

func (poller *FeatureFlagsPoller) IsFeatureEnabled(key string, distinctId string, defaultResult bool) (bool, error) {
	featureFlags := poller.GetFeatureFlags()

	if len(featureFlags) < 1 {
		return defaultResult, nil
	}

	featureFlag := FeatureFlag{Key: ""}

	// avoid using flag for conflicts with Golang's stdlib `flag`
	for _, storedFlag := range featureFlags {
		if key == storedFlag.Key {
			featureFlag = storedFlag
			break
		}
	}

	if featureFlag.Key == "" {
		return defaultResult, nil
	}

	result, err := poller.getFeatureFlagValue(featureFlag, key, distinctId)
	if err != nil {
		return false, err
	}
	var flagValueString = fmt.Sprintf("%v", result)
	if flagValueString != "false" {
		return true, nil
	}
	return false, nil
}

func (poller *FeatureFlagsPoller) GetFeatureFlag(key string, distinctId string, defaultResult interface{}) (interface{}, error) {
	featureFlags := poller.GetFeatureFlags()

	if len(featureFlags) < 1 {
		return defaultResult, nil
	}

	featureFlag := FeatureFlag{Key: ""}

	// avoid using flag for conflicts with Golang's stdlib `flag`
	for _, storedFlag := range featureFlags {
		if key == storedFlag.Key {
			featureFlag = storedFlag
			break
		}
	}

	if featureFlag.Key == "" {
		return defaultResult, nil
	}

	return poller.getFeatureFlagValue(featureFlag, key, distinctId)
}

func (poller *FeatureFlagsPoller) isSimpleFlagEnabled(key string, distinctId string, rolloutPercentage uint8) (bool, error) {
	isEnabled, err := checkIfSimpleFlagEnabled(key, distinctId, rolloutPercentage)
	if err != nil {
		errMessage := "Error converting string to int"
		poller.Errorf(errMessage)
		return false, errors.New(errMessage)
	}
	return isEnabled, nil
}

// extracted as a regular func for testing purposes
func checkIfSimpleFlagEnabled(key string, distinctId string, rolloutPercentage uint8) (bool, error) {
	hash := sha1.New()
	hash.Write([]byte("" + key + "." + distinctId + ""))
	digest := hash.Sum(nil)
	hexString := fmt.Sprintf("%x\n", digest)[:15]

	value, err := strconv.ParseInt(hexString, 16, 64)
	if err != nil {
		return false, err
	}
	return (float64(value) / LONG_SCALE) <= float64(rolloutPercentage)/100, nil
}

func (poller *FeatureFlagsPoller) GetFeatureFlags() []FeatureFlag {
	// ensure flags are loaded on the first call
	if !poller.fetchedFlagsSuccessfullyOnce {
		<-poller.loaded
	}

	poller.mutex.RLock()

	defer poller.mutex.RUnlock()

	return poller.featureFlags
}

func (poller *FeatureFlagsPoller) request(method string, endpoint string, requestData []byte, headers [][2]string) (*http.Response, error) {

	url, err := url.Parse(poller.Endpoint + "/" + endpoint + "")

	if err != nil {
		poller.Errorf("creating url - %s", err)
	}
	searchParams := url.Query()

	if method == "GET" {
		searchParams.Add("token", poller.projectApiKey)
	}

	if endpoint == "decide" {
		searchParams.Add("v", "2")
	}
	url.RawQuery = searchParams.Encode()

	req, err := http.NewRequest(method, url.String(), bytes.NewReader(requestData))
	if err != nil {
		poller.Errorf("creating request - %s", err)
	}

	version := getVersion()

	req.Header.Add("User-Agent", "posthog-go (version: "+version+")")
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Content-Length", fmt.Sprintf("%d", len(requestData)))

	for _, header := range headers {
		req.Header.Add(header[0], header[1])
	}

	res, err := poller.http.Do(req)

	if err != nil {
		poller.Errorf("sending request - %s", err)
	}

	return res, err
}

func (poller *FeatureFlagsPoller) ForceReload() {
	poller.forceReload <- true
}

func (poller *FeatureFlagsPoller) shutdownPoller() {
	poller.shutdown <- true
}

func (poller *FeatureFlagsPoller) getFeatureFlagValue(featureFlag FeatureFlag, key string, distinctId string) (interface{}, error) {
	var result interface{} = false
	errorMessage := "Failed when getting flag value"

	if featureFlag.IsSimpleFlag {

		// json.Unmarshal will convert JSON `null` to a nullish value for each type
		// which is 0 for uint. However, our feature flags should have rolloutPercentage == 100
		// if it is set to `null`. Having rollout percentage be a pointer and deferencing it
		// here allows its value to be `nil` following json.Unmarhsal, so we can appropriately
		// set it to 100
		rolloutPercentage := uint8(100)
		if featureFlag.RolloutPercentage != nil {
			rolloutPercentage = *featureFlag.RolloutPercentage
		}
		var err error
		result, err = poller.isSimpleFlagEnabled(key, distinctId, rolloutPercentage)
		if err != nil {
			return false, err
		}
	} else {
		requestDataBytes, err := json.Marshal(DecideRequestData{
			ApiKey:     poller.projectApiKey,
			DistinctId: distinctId,
		})
		if err != nil {
			errorMessage = "unable to marshal decide endpoint request data"
			poller.Errorf(errorMessage)
			return false, errors.New(errorMessage)
		}
		res, err := poller.request("POST", "decide", requestDataBytes, [][2]string{})
		if err != nil || res.StatusCode != http.StatusOK {
			errorMessage = "Error calling /decide/"
			poller.Errorf(errorMessage)
			return false, errors.New(errorMessage)
		}
		resBody, err := ioutil.ReadAll(res.Body)
		if err != nil {
			errorMessage = "Error reading response from /decide/"
			poller.Errorf(errorMessage)
			return false, errors.New(errorMessage)
		}
		defer res.Body.Close()
		decideResponse := DecideResponse{}
		err = json.Unmarshal([]byte(resBody), &decideResponse)
		if err != nil {
			errorMessage = "Error parsing response from /decide/"
			poller.Errorf(errorMessage)
			return false, errors.New(errorMessage)
		}
		for flagKey, flagValue := range decideResponse.FeatureFlags {
			var flagValueString = fmt.Sprintf("%v", flagValue)
			if key == flagKey && flagValueString != "false" {
				result = flagValueString
				break
			}
		}
		return result, nil
	}
	return result, nil
}
