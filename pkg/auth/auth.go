package auth

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"

	"k8s.io/klog"

	"github.com/Azure/go-autorest/autorest/adal"
	"github.com/Azure/secrets-store-csi-driver-provider-azure/pkg/utils"
	"github.com/pkg/errors"
)

const (
	// Pod Identity nmiEndpoint
	nmiEndpoint = "http://localhost:2579/host/token/"
	// Pod Identity podNameHeader
	podNameHeader = "podname"
	// Pod Identity podNamespaceHeader
	podNamespaceHeader = "podns"
)

// NMIResponse is the response received from aad-pod-identity when requesting token
// on behalf of the pod
type NMIResponse struct {
	Token    adal.Token `json:"token"`
	ClientID string     `json:"clientid"`
}

// Config is the required parameters for auth config
type Config struct {
	// UsePodIdentity is set to true if access mode is using aad-pod-identity
	UsePodIdentity bool
	// UseVMManagedIdentity is set to true if access mode is using managed identity
	UseVMManagedIdentity bool
	// UserAssignedIdentityID is the user-assigned managed identity clientID
	UserAssignedIdentityID string
	// AADClientSecret is the client secret for SP access mode
	AADClientSecret string
	// AADClientID is the clientID for SP access mode
	AADClientID string
}

// NewConfig returns new auth config
func NewConfig(usePodIdentity, useVMManagedIdentity bool, userAssignedIdentityID string, secrets map[string]string) (Config, error) {
	config := Config{}
	// aad-pod-identity and user assigned managed identity modes are currently mutually exclusive
	if usePodIdentity && useVMManagedIdentity {
		return config, fmt.Errorf("cannot enable both pod identity and user-assigned managed identity")
	}
	if !usePodIdentity && !useVMManagedIdentity {
		var err error
		if config.AADClientID, config.AADClientSecret, err = getCredential(secrets); err != nil {
			return config, err
		}
	}

	config.UsePodIdentity = usePodIdentity
	config.UseVMManagedIdentity = useVMManagedIdentity
	config.UserAssignedIdentityID = userAssignedIdentityID

	return config, nil
}

func (c Config) GetServicePrincipalToken(podName, podNamespace, resource, aadEndpoint, tenantID string) (*adal.ServicePrincipalToken, error) {
	oauthConfig, err := adal.NewOAuthConfig(aadEndpoint, tenantID)
	if err != nil {
		return nil, fmt.Errorf("failed to create OAuth config: %v", err)
	}

	// For usePodIdentity mode, the CSI driver makes an authorization request to fetch token for a resource from the NMI host endpoint (http://127.0.0.1:2579/host/token/).
	// The request includes the pod namespace `podns` and the pod name `podname` in the request header and the resource endpoint of the resource requesting the token.
	// The NMI server identifies the pod based on the `podns` and `podname` in the request header and then queries k8s (through MIC) for a matching azure identity.
	// Then nmi makes an adal request to get a token for the resource in the request, returns the `token` and the `clientid` as a response to the CSI request.
	if c.UsePodIdentity {
		klog.Infof("azure: using pod identity to retrieve token")
		// pod name and namespace are required for the Key Vault provider to request a token
		// on behalf of the application pod
		if len(podName) == 0 || len(podNamespace) == 0 {
			return nil, fmt.Errorf("pod information is not available. deploy a CSIDriver object to set podInfoOnMount: true")
		}

		endpoint := fmt.Sprintf("%s?resource=%s", nmiEndpoint, resource)
		client := &http.Client{}
		req, err := http.NewRequest(http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Add(podNamespaceHeader, podNamespace)
		req.Header.Add(podNameHeader, podName)
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("nmi response failed with status code: %d", resp.StatusCode)
		}

		bodyBytes, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		var nmiResp = new(NMIResponse)
		err = json.Unmarshal(bodyBytes, &nmiResp)
		if err != nil {
			return nil, err
		}

		klog.Infof("access token: %s, clientID: %s", utils.RedactClientID(nmiResp.Token.AccessToken), utils.RedactClientID(nmiResp.ClientID))

		token := nmiResp.Token
		clientID := nmiResp.ClientID

		if token.AccessToken == "" || clientID == "" {
			return nil, fmt.Errorf("nmi did not return expected values in response: token and clientid")
		}

		spt, err := adal.NewServicePrincipalTokenFromManualToken(*oauthConfig, clientID, resource, token, nil)
		if err != nil {
			return nil, err
		}
		return spt, nil
	}

	if c.UseVMManagedIdentity {
		msiEndpoint, err := adal.GetMSIVMEndpoint()
		if err != nil {
			return nil, errors.Wrap(err, "failed to get managed identity (MSI) endpoint")
		}
		if c.UserAssignedIdentityID != "" {
			klog.Infof("azure: using user-assigned managed identity %s to retrieve access token", utils.RedactClientID(c.UserAssignedIdentityID))
			return adal.NewServicePrincipalTokenFromMSIWithUserAssignedID(
				msiEndpoint,
				resource,
				c.UserAssignedIdentityID)
		}

		klog.Infof("azure: using system-assigned managed identity to retrieve access token")
		return adal.NewServicePrincipalTokenFromMSI(
			msiEndpoint,
			resource)
	}

	// for Service Principal access mode, clientID + client secret are used to retrieve token for resource
	if len(c.AADClientSecret) > 0 {
		klog.Infof("azure: using client_id: %s, client_secret: %s to retrieve access token", utils.RedactClientID(c.AADClientID), utils.RedactClientID(c.AADClientSecret))
		return adal.NewServicePrincipalToken(
			*oauthConfig,
			c.AADClientID,
			c.AADClientSecret,
			resource)
	}
	return nil, fmt.Errorf("no valid credentials")
}

// getCredential gets clientid and clientsecret from the secrets
func getCredential(secrets map[string]string) (string, string, error) {
	if secrets == nil {
		return "", "", fmt.Errorf("failed to get credentials, nodePublishSecretRef secret is not set")
	}

	var clientID, clientSecret string
	for k, v := range secrets {
		switch strings.ToLower(k) {
		case "clientid":
			clientID = v
		case "clientsecret":
			clientSecret = v
		}
	}

	if clientID == "" {
		return "", "", fmt.Errorf("could not find clientid in secrets(%v)", secrets)
	}
	if clientSecret == "" {
		return "", "", fmt.Errorf("could not find clientsecret in secrets(%v)", secrets)
	}
	return clientID, clientSecret, nil
}
