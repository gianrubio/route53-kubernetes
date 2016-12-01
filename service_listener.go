package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/credentials/ec2rolecreds"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/elb"
	"github.com/aws/aws-sdk-go/service/route53"
	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/client/restclient"
	"k8s.io/kubernetes/pkg/client/transport"
	client "k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/labels"
	"k8s.io/kubernetes/pkg/apis/extensions"
)

// Don't actually commit the changes to route53 records, just print out what we would have done.
var dryRun bool

func init() {
	dryRunStr := os.Getenv("DRY_RUN")
	if dryRunStr != "" {
		dryRun = true
	}
}

func main() {
	flag.Parse()
	glog.Info("Route53 Update Service")

	config, err := restclient.InClusterConfig()
	if err != nil {
		kubernetesService := os.Getenv("KUBERNETES_SERVICE_HOST")
		kubernetesServicePort := os.Getenv("KUBERNETES_SERVICE_PORT")
		if kubernetesService == "" {
			glog.Fatal("Please specify the Kubernetes server via KUBERNETES_SERVICE_HOST")
		}
		if kubernetesServicePort == "" {
			kubernetesServicePort = "443"
		}
		apiServer := fmt.Sprintf("https://%s:%s", kubernetesService, kubernetesServicePort)

		caFilePath := os.Getenv("CA_FILE_PATH")
		certFilePath := os.Getenv("CERT_FILE_PATH")
		keyFilePath := os.Getenv("KEY_FILE_PATH")
		if caFilePath == "" || certFilePath == "" || keyFilePath == "" {
			glog.Fatal("You must provide paths for CA, Cert, and Key files")
		}

		tls := transport.TLSConfig{
			CAFile:   caFilePath,
			CertFile: certFilePath,
			KeyFile:  keyFilePath,
		}
		// tlsTransport := transport.New(transport.Config{TLS: tls})
		tlsTransport, err := transport.New(&transport.Config{TLS: tls})
		if err != nil {
			glog.Fatalf("Couldn't set up tls transport: %s", err)
		}

		config = &restclient.Config{
			Host:      apiServer,
			Transport: tlsTransport,
		}
	}

	c, err := client.New(config)
	if err != nil {
		glog.Fatalf("Failed to make client: %v", err)
	}
	glog.Infof("Connected to kubernetes @ %s", config.Host)

	awsConfig := aws.NewConfig()

	awsAccessKeyId := os.Getenv("AWS_ACCESS_KEY_ID")
	awsSecretAccessKey := os.Getenv("AWS_SECRET_ACCESS_KEY")
	region := os.Getenv("AWS_REGION")

	// aws credentials based on role
	if awsAccessKeyId == "" || awsSecretAccessKey == "" || region == ""{

		metadata := ec2metadata.New(session.New())

		creds := credentials.NewChainCredentials(
			[]credentials.Provider{
				&credentials.EnvProvider{},
				&credentials.SharedCredentialsProvider{},
				&ec2rolecreds.EC2RoleProvider{Client: metadata},
			})

		region, err = metadata.Region()
		if err != nil {
			glog.Fatalf("Unable to retrieve the region from the EC2 instance %v\n", err)
		}


		awsConfig.WithCredentials(creds)
	}

	awsConfig.WithRegion(region)
	sess := session.New(awsConfig)

	r53Api := route53.New(sess)
	elbAPI := elb.New(sess)
	if r53Api == nil || elbAPI == nil {
		glog.Fatal("Failed to make AWS connection")
	}

	selector := "dns=route53"
	l, err := labels.Parse(selector)
	if err != nil {
		glog.Fatalf("Failed to parse selector %q: %v", selector, err)
	}
	listOptions := api.ListOptions{
		LabelSelector: l,
	}

	glog.Infof("Starting Service Polling every 30s")
	for {
		ingresses, err := c.Ingress(api.NamespaceAll).List(listOptions)
		if err != nil {
			glog.Fatalf("Failed to list pods: %v", err)
		}

		glog.Infof("Found %d DNS ingresses in all namespaces with selector %q", len(ingresses.Items), selector)
		for i := range ingresses.Items {
			s := &ingresses.Items[i]
			hn, err := ingressHostname(s)
			if err != nil {
				glog.Warningf("Couldn't find hostname for %s: %s", s.Name, err)
				continue
			}

			internalElbName, ok := s.ObjectMeta.Annotations["internalElbName"]

			if !ok {
				glog.Warningf("Internal elb name not set for %s", s.Name)
				continue
			}

			externalElbName, ok := s.ObjectMeta.Annotations["externalElbName"]

			if !ok {
				glog.Warningf("External elb name not set for %s", s.Name)
				continue
			}

			domains := strings.Split(hn, ",")
			for j := range domains {

				for _, isPrivate := range [2]bool{true, false}  {

					domain := domains[j]
					elbName := externalElbName

					if isPrivate {
						elbName = internalElbName
					}

					elbZoneID, err := hostedZoneID(elbAPI, elbName)
					if err != nil {
						glog.Warningf("Couldn't get zone ID: %s", err)
						continue
					}

					zone, err := getDestinationZone(domain, isPrivate, r53Api)
					if err != nil {
						glog.Warningf("Couldn't find destination zone: %s", err)
						continue
					}

					zoneID := *zone.Id
					zoneParts := strings.Split(zoneID, "/")
					zoneID = zoneParts[len(zoneParts)-1]

					if err = updateDNS(r53Api, elbName, elbZoneID, strings.TrimLeft(domain, "."), zoneID); err != nil {
						glog.Warning(err)
						continue
					}

				}
			}
		}
		time.Sleep(15 * time.Second)
	}
}

func getDestinationZone(domain string, isPrivateZone bool, r53Api *route53.Route53) (*route53.HostedZone, error) {
	tld, err := getTLD(domain)
	if err != nil {
		return nil, err
	}

	listHostedZoneInput := route53.ListHostedZonesByNameInput{
		DNSName: &tld,
	}
	hzOut, err := r53Api.ListHostedZonesByName(&listHostedZoneInput)
	if err != nil {
		return nil, fmt.Errorf("No zone found for %s: %v", tld, err)
	}
	// TODO: The AWS API may return multiple pages, we should parse them all

	return findMostSpecificZoneForDomain(domain, isPrivateZone, hzOut.HostedZones)
}

func findMostSpecificZoneForDomain(domain string, isPrivateZone bool, zones []*route53.HostedZone) (*route53.HostedZone, error) {
	domain = domainWithTrailingDot(domain)
	if len(zones) < 1 {
		return nil, fmt.Errorf("No zone found for %s", domain)
	}
	var mostSpecific *route53.HostedZone
	curLen := 0

	for i := range zones {
		zone := zones[i]
		zoneName := *zone.Name
		zoneIsPrivate := *zone.Config.PrivateZone

		if strings.HasSuffix(domain, zoneName) && zoneIsPrivate == isPrivateZone && curLen < len(zoneName) {
			curLen = len(zoneName)
			mostSpecific = zone
		}
	}

	if mostSpecific == nil {
		return nil, fmt.Errorf("Zone found %s does not match domain given %s", *zones[0].Name, domain)
	}

	return mostSpecific, nil
}

func getTLD(domain string) (string, error) {
	domainParts := strings.Split(domain, ".")
	segments := len(domainParts)
	if segments < 3 {
		return "", fmt.Errorf("Domain %s is invalid - it should be a fully qualified domain name and subdomain (i.e. test.example.com)", domain)
	}
	return strings.Join(domainParts[segments-2:], "."), nil
}

func domainWithTrailingDot(withoutDot string) string {
	if withoutDot[len(withoutDot)-1:] == "." {
		return withoutDot
	}
	return fmt.Sprint(withoutDot, ".")
}

func serviceHostname(service *api.Service) (string, error) {
	ingress := service.Status.LoadBalancer.Ingress
	if len(ingress) < 1 {
		return "", errors.New("No ingress defined for ELB")
	}
	if len(ingress) > 1 {
		return "", errors.New("Multiple ingress points found for ELB, not supported")
	}
	return ingress[0].Hostname, nil
}

func ingressHostname(ingress *extensions.Ingress) (string, error) {
	xb := ingress.Spec.Rules
	if len(xb) < 1 {
		return "", errors.New("No ingress defined for ELB")
	}
	if len(xb) > 1 {
		return "", errors.New("Multiple ingress points found for ELB, not supported")
	}
	glog.Infof("Hostname found: %s", xb[0].Host)
	return xb[0].Host, nil
}

func loadBalancerNameFromHostname(hostname string) (string, error) {
	var name string
	hostnameSegments := strings.Split(hostname, "-")
	if len(hostnameSegments) < 2 {
		return name, fmt.Errorf("%s is not a valid ELB hostname", hostname)
	}
	name = hostnameSegments[0]

	// handle internal load balancer naming
	if name == "internal" {
		name = strings.Join(hostnameSegments[1:4],"-")
	}

	return name, nil
}

func hostedZoneID(elbAPI *elb.ELB, hostname string) (string, error) {

	elbName, err := loadBalancerNameFromHostname(hostname)

	if err != nil {
		return "", fmt.Errorf("Couldn't parse ELB hostname: %v", err)
	}
	lbInput := &elb.DescribeLoadBalancersInput{
		LoadBalancerNames: []*string{
			&elbName,
		},
	}
	resp, err := elbAPI.DescribeLoadBalancers(lbInput)
	if err != nil {
		return "", fmt.Errorf("Could not describe load balancer: %v", err)
	}
	descs := resp.LoadBalancerDescriptions
	if len(descs) < 1 {
		return "", fmt.Errorf("No lb found: %v", err)
	}
	if len(descs) > 1 {
		return "", fmt.Errorf("Multiple lbs found: %v", err)
	}
	return *descs[0].CanonicalHostedZoneNameID, nil
}

func updateDNS(r53Api *route53.Route53, hn, hzID, domain, zoneID string) error {

	listParams := &route53.ListResourceRecordSetsInput{
		HostedZoneId: &zoneID,
		StartRecordName: &domain,
		StartRecordType: aws.String("A"),
	}

	respList, err := r53Api.ListResourceRecordSets(listParams)
	elbHostname := &hn

	if len(respList.ResourceRecordSets) > 0 && strings.EqualFold(strings.Trim(*respList.ResourceRecordSets[0].AliasTarget.DNSName, "."), *elbHostname) {
		glog.Infof("No changes in route53 recordset %s", hn)
		return nil

	}else{

		at := route53.AliasTarget{
		DNSName:              &hn,
		EvaluateTargetHealth: aws.Bool(false),
		HostedZoneId:         &hzID,
		}
		rrs := route53.ResourceRecordSet{
			AliasTarget: &at,
			Name:        &domain,
			Type:        aws.String("A"),
		}
		change := route53.Change{
			Action:            aws.String("UPSERT"),
			ResourceRecordSet: &rrs,
		}
		batch := route53.ChangeBatch{
			Changes: []*route53.Change{&change},
			Comment: aws.String("Kubernetes Update to Service"),
		}

		crrsInput := route53.ChangeResourceRecordSetsInput{
			ChangeBatch:  &batch,
			HostedZoneId: &zoneID,
		}
		if dryRun {
			glog.Infof("DRY RUN: We normally would have updated %s to point to %s (%s)", zoneID, hzID, hn)
			return nil
		}else{
			glog.Infof("Creating %s recordset %s to point to %s (%s)", zoneID, hzID, hn)

		}

		glog.Infof("Created dns record set: domain=%s, zoneID=%s", domain, zoneID)


		_, err = r53Api.ChangeResourceRecordSets(&crrsInput)
		if err != nil {
			return fmt.Errorf("Failed to update record set: %v", err)
		}
	}
	return nil
}
