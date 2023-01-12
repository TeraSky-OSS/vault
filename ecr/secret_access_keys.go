package aws

import (
	"context"
	"fmt"
	"regexp"
	// "time"

	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-secure-stdlib/awsutil"
	"github.com/hashicorp/vault/sdk/framework"
	"github.com/hashicorp/vault/sdk/helper/template"
	"github.com/hashicorp/vault/sdk/logical"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/aws/aws-sdk-go/service/ecr/ecriface"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/hashicorp/errwrap"
)

const (
	secretAccessKeyType        = "access_keys"
	storageKey                 = "config/root"
	registryPermissionReadArn  = "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly"
	registryPermissionWriteArn = "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryPowerUser"
)

func secretAccessKeys(b *backend) *framework.Secret {
	return &framework.Secret{
		Type: secretAccessKeyType,
		Fields: map[string]*framework.FieldSchema{
			"access_key": {
				Type:        framework.TypeString,
				Description: "Access Key",
			},
			"secret_key": {
				Type:        framework.TypeString,
				Description: "Secret Key",
			},
		},

		Revoke: b.secretAccessKeysRevoke,
	}
}

func genUsername(displayName, policyName, usernameTemplate string) (ret string, err error) {
	// IAM users are capped at 64 chars
	up, err := template.NewTemplate(template.Template(usernameTemplate))
	if err != nil {
		return "", fmt.Errorf("unable to initialize username template: %w", err)
	}

	um := UsernameMetadata{
		Type:        "IAM",
		DisplayName: normalizeDisplayName(displayName),
		PolicyName:  normalizeDisplayName(policyName),
	}

	ret, err = up.Generate(um)
	if err != nil {
		return "", fmt.Errorf("failed to generate username: %w", err)
	}
	// To prevent a custom template from exceeding IAM length limits
	if len(ret) > 64 {
		return "", fmt.Errorf("the username generated by the template exceeds the IAM username length limits of 64 chars")
	}
	return
}

func getAuthorizationTokenHelper(ecrClient ecriface.ECRAPI, maxRetries int, startCount int, logger hclog.Logger) (*ecr.GetAuthorizationTokenOutput, error) {
	getTokenInput := &ecr.GetAuthorizationTokenInput{}
	tokenResp, err := ecrClient.GetAuthorizationToken(getTokenInput)
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			if aerr.Code() == "UnrecognizedClientException" &&
				aerr.Message() == "The security token included in the request is invalid." &&
				startCount < maxRetries {
				startCount++
				logger.Info(fmt.Sprintf("Failed to retrieve ECR token, tried (%d) of (%d).", startCount, maxRetries))
				return getAuthorizationTokenHelper(ecrClient, maxRetries, startCount, logger)
			}
		}
		return nil, err
	}
	return tokenResp, nil
}

func (b *backend) getAuthorizationToken(ctx context.Context, s logical.Storage, auth *logical.Response) (*logical.Response, error) {
	ecrClient, err := b.clientECR(ctx, s, auth)

	var maxRetries int = 30
	tokenResp, err := getAuthorizationTokenHelper(ecrClient, maxRetries, 0, b.Logger())
	if err != nil {
		return nil, fmt.Errorf("Error generating ECR token: %s\nCheckAWSError: %s", err, awsutil.CheckAWSError(err))
	}

	return &logical.Response{
		Data: map[string]interface{}{
			"auth_token":   *tokenResp.AuthorizationData[0].AuthorizationToken,
			"registry_url": *tokenResp.AuthorizationData[0].ProxyEndpoint,
		},
		Secret: auth.Secret,
	}, nil
}

func readConfig(ctx context.Context, storage logical.Storage) (rootConfig, error) {
	entry, err := storage.Get(ctx, storageKey)
	if err != nil {
		return rootConfig{}, err
	}
	if entry == nil {
		return rootConfig{}, nil
	}

	var connConfig rootConfig
	if err := entry.DecodeJSON(&connConfig); err != nil {
		return rootConfig{}, err
	}
	return connConfig, nil
}

func (b *backend) secretAccessKeysCreate(
	ctx context.Context,
	s logical.Storage,
	displayName, policyName string,
	role *awsRoleEntry, lifeTimeInSeconds int64,
) (*logical.Response, error) {
	iamClient, err := b.clientIAM(ctx, s)
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	config, err := readConfig(ctx, s)
	if err != nil {
		return nil, fmt.Errorf("unable to read configuration: %w", err)
	}

	// Set as defaultUsernameTemplate if not provided
	usernameTemplate := config.UsernameTemplate
	if usernameTemplate == "" {
		usernameTemplate = defaultUserNameTemplate
	}

	username, usernameError := genUsername(displayName, policyName, usernameTemplate)
	// Send a 400 to Framework.OperationFunc Handler
	if usernameError != nil {
		return nil, usernameError
	}

	// Write to the WAL that this user will be created. We do this before
	// the user is created because if switch the order then the WAL put
	// can fail, which would put us in an awkward position: we have a user
	// we need to rollback but can't put the WAL entry to do the rollback.
	walID, err := framework.PutWAL(ctx, s, "user", &walUser{
		UserName: username,
	})
	if err != nil {
		return nil, fmt.Errorf("error writing WAL entry: %w", err)
	}

	userPath := fmt.Sprintf("/%s/", username)

	createUserRequest := &iam.CreateUserInput{
		UserName: aws.String(username),
		Path:     aws.String(userPath),
	}

	// Create the user
	_, err = iamClient.CreateUser(createUserRequest)
	if err != nil {
		if walErr := framework.DeleteWAL(ctx, s, walID); walErr != nil {
			iamErr := fmt.Errorf("error creating IAM user: %w", err)
			return nil, errwrap.Wrap(fmt.Errorf("failed to delete WAL entry: %w", walErr), iamErr)
		}
		return logical.ErrorResponse("Error creating IAM user: %s", err), awsutil.CheckAWSError(err)
	}

	resp := b.Secret(secretAccessKeyType).Response(map[string]interface{}{}, map[string]interface{}{
		"username": username,
	})

	arn := ""
	switch role.RegistryPermission {
	case "read":
		arn = registryPermissionReadArn
	case "write":
		arn = registryPermissionWriteArn
	}
	// Attach existing policy against user
	_, err = iamClient.AttachUserPolicy(&iam.AttachUserPolicyInput{
		UserName:  aws.String(username),
		PolicyArn: aws.String(arn),
	})
	if err != nil {
		return resp, fmt.Errorf("Error attaching user policy: %s. %s", err, awsutil.CheckAWSError(err))
	}

	resp.Secret.InternalData["policy"] = role

	// TODO
	// var tags []*iam.Tag
	// for key, value := range role.IAMTags {
	// 	// This assignment needs to be done in order to create unique addresses for
	// 	// these variables. Without doing so, all the tags will be copies of the last
	// 	// tag listed in the role.
	// 	k, v := key, value
	// 	tags = append(tags, &iam.Tag{Key: &k, Value: &v})
	// }

	// if len(tags) > 0 {
	// 	_, err = iamClient.TagUser(&iam.TagUserInput{
	// 		Tags:     tags,
	// 		UserName: &username,
	// 	})

	// 	if err != nil {
	// 		return logical.ErrorResponse("Error adding tags to user: %s", err), awsutil.CheckAWSError(err)
	// 	}
	// }

	// Create the keys
	keyResp, err := iamClient.CreateAccessKey(&iam.CreateAccessKeyInput{
		UserName: aws.String(username),
	})
	if err != nil {
		return resp, fmt.Errorf("Error creating access keys: %s. %s", err, awsutil.CheckAWSError(err))
	}

	resp.Secret.InternalData["access_key"] = *keyResp.AccessKey.AccessKeyId
	resp.Secret.InternalData["secret_key"] = *keyResp.AccessKey.SecretAccessKey

	// Remove the WAL entry, we succeeded! If we fail, we don't return
	// the secret because it'll get rolled back anyways, so we have to return
	// an error here.
	if err := framework.DeleteWAL(ctx, s, walID); err != nil {
		return resp, fmt.Errorf("failed to commit WAL entry: %w", err)
	}

	lease, err := b.Lease(ctx, s, lifeTimeInSeconds)
	// if err != nil || lease == nil {
	// 	lease = &configLease{}
	// }
	if err != nil {
		return resp, err
	}

	resp.Secret.TTL = lease.Lease
	resp.Secret.MaxTTL = lease.LeaseMax

	return resp, nil
}

func (b *backend) secretAccessKeysRevoke(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	// Get the username from the internal data
	usernameRaw, ok := req.Secret.InternalData["username"]
	if !ok {
		return nil, fmt.Errorf("secret is missing username internal data")
	}
	username, ok := usernameRaw.(string)
	if !ok {
		return nil, fmt.Errorf("secret is missing username internal data")
	}

	// Use the user rollback mechanism to delete this user
	err := b.pathUserRollback(ctx, req, "user", map[string]interface{}{
		"username": username,
	})
	if err != nil {
		return nil, err
	}

	return nil, nil
}

func normalizeDisplayName(displayName string) string {
	re := regexp.MustCompile("[^a-zA-Z0-9+=,.@_-]")
	return re.ReplaceAllString(displayName, "_")
}

type UsernameMetadata struct {
	Type        string
	DisplayName string
	PolicyName  string
}
