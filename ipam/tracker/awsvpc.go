package tracker

// TODO(mp) docs

import (
	"fmt"
	"net"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/vishvananda/netlink"

	"github.com/weaveworks/weave/common"
	wnet "github.com/weaveworks/weave/net"
	"github.com/weaveworks/weave/net/address"
)

type AWSVPCTracker struct {
	ec2          *ec2.EC2
	instanceID   string // EC2 Instance ID
	routeTableID string // VPC Route Table ID
	linkIndex    int    // The weave bridge link index
}

// NewAWSVPCTracker creates and initialises AWS VPC based tracker.
//
// The tracker updates AWS VPC and host route tables when any changes to allocated
// address ranges owned by a peer have been done.
func NewAWSVPCTracker() (*AWSVPCTracker, error) {
	var (
		err     error
		session = session.New()
		t       = &AWSVPCTracker{}
	)

	// Detect region and instance id
	meta := ec2metadata.New(session)
	t.instanceID, err = meta.GetMetadata("instance-id")
	if err != nil {
		return nil, fmt.Errorf("cannot detect instance-id: %s", err)
	}
	region, err := meta.Region()
	if err != nil {
		return nil, fmt.Errorf("cannot detect region: %s", err)
	}

	t.ec2 = ec2.New(session, aws.NewConfig().WithRegion(region))

	routeTableID, err := t.detectRouteTableID()
	if err != nil {
		return nil, err
	}
	t.routeTableID = *routeTableID

	// Detect Weave bridge link index
	link, err := netlink.LinkByName(wnet.WeaveBridgeName)
	if err != nil {
		return nil, fmt.Errorf("cannot find \"%s\" interface: %s", wnet.WeaveBridgeName, err)
	}
	t.linkIndex = link.Attrs().Index

	t.infof("AWSVPC has been initialized on %s instance for %s route table at %s region",
		t.instanceID, t.routeTableID, region)

	return t, nil
}

// HandleUpdate method updates the AWS VPC and the host route tables.
func (t *AWSVPCTracker) HandleUpdate(prevRanges, currRanges []address.Range) error {
	t.debugf("replacing %q entries by %q", prevRanges, currRanges)

	prev, curr := removeCommon(address.NewCIDRs(prevRanges), address.NewCIDRs(currRanges))

	// It might make sense to do the removal first and then add entries
	// because of the 50 routes limit. However, in such case a container might
	// not be reachable for a short period of time which is not a desired behavior.

	// Add new entries
	for _, cidr := range curr {
		cidrStr := cidr.String()
		t.debugf("adding route %s to %s", cidrStr, t.instanceID)
		_, err := t.createVPCRoute(cidrStr)
		// TODO(mp) check for 50 routes limit
		// TODO(mp) maybe check for auth related errors
		if err != nil {
			return fmt.Errorf("createVPCRoutes failed: %s", err)
		}
		err = t.createHostRoute(cidrStr)
		if err != nil {
			return fmt.Errorf("createHostRoute failed: %s", err)
		}
	}

	// Remove obsolete entries
	for _, cidr := range prev {
		cidrStr := cidr.String()
		t.debugf("removing %s route", cidrStr)
		_, err := t.deleteVPCRoute(cidrStr)
		if err != nil {
			return fmt.Errorf("deleteVPCRoute failed: %s", err)
		}
		err = t.deleteHostRoute(cidrStr)
		if err != nil {
			return fmt.Errorf("deleteHostRoute failed: %s", err)
		}
	}

	return nil
}

func (t *AWSVPCTracker) String() string {
	return "awsvpc"
}

func (t *AWSVPCTracker) createVPCRoute(cidr string) (*ec2.CreateRouteOutput, error) {
	route := &ec2.CreateRouteInput{
		RouteTableId:         &t.routeTableID,
		InstanceId:           &t.instanceID,
		DestinationCidrBlock: &cidr,
	}
	return t.ec2.CreateRoute(route)
}

func (t *AWSVPCTracker) createHostRoute(cidr string) error {
	dst, err := parseCIDR(cidr)
	if err != nil {
		return err
	}
	route := &netlink.Route{
		LinkIndex: t.linkIndex,
		Dst:       dst,
		Scope:     netlink.SCOPE_LINK,
	}
	return netlink.RouteAdd(route)
}

func (t *AWSVPCTracker) deleteVPCRoute(cidr string) (*ec2.DeleteRouteOutput, error) {
	route := &ec2.DeleteRouteInput{
		RouteTableId:         &t.routeTableID,
		DestinationCidrBlock: &cidr,
	}
	return t.ec2.DeleteRoute(route)
}

func (t *AWSVPCTracker) deleteHostRoute(cidr string) error {
	dst, err := parseCIDR(cidr)
	if err != nil {
		return err
	}
	route := &netlink.Route{
		LinkIndex: t.linkIndex,
		Dst:       dst,
		Scope:     netlink.SCOPE_LINK,
	}
	return netlink.RouteDel(route)
}

// detectRouteTableID detects AWS VPC Route Table ID of the given tracker instance.
func (t *AWSVPCTracker) detectRouteTableID() (*string, error) {
	instancesParams := &ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String(t.instanceID)},
	}
	instancesResp, err := t.ec2.DescribeInstances(instancesParams)
	if err != nil {
		return nil, fmt.Errorf("DescribeInstances failed: %s", err)
	}
	if len(instancesResp.Reservations) == 0 ||
		len(instancesResp.Reservations[0].Instances) == 0 {
		return nil, fmt.Errorf("cannot find %s instance within reservations", t.instanceID)
	}
	vpcID := instancesResp.Reservations[0].Instances[0].VpcId
	subnetID := instancesResp.Reservations[0].Instances[0].SubnetId

	// First try to find a routing table associated with the subnet of the instance
	tablesParams := &ec2.DescribeRouteTablesInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("association.subnet-id"),
				Values: []*string{subnetID},
			},
		},
	}
	tablesResp, err := t.ec2.DescribeRouteTables(tablesParams)
	if err != nil {
		return nil, fmt.Errorf("DescribeRouteTables failed: %s", err)
	}
	if len(tablesResp.RouteTables) != 0 {
		return tablesResp.RouteTables[0].RouteTableId, nil
	}
	// Fallback to the default routing table
	tablesParams = &ec2.DescribeRouteTablesInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("association.main"),
				Values: []*string{aws.String("true")},
			},
			{
				Name:   aws.String("vpc-id"),
				Values: []*string{vpcID},
			},
		},
	}
	tablesResp, err = t.ec2.DescribeRouteTables(tablesParams)
	if err != nil {
		return nil, fmt.Errorf("DescribeRouteTables failed: %s", err)
	}
	if len(tablesResp.RouteTables) != 0 {
		return tablesResp.RouteTables[0].RouteTableId, nil
	}

	return nil, fmt.Errorf("cannot find routetable for %s instance", t.instanceID)
}

func (t *AWSVPCTracker) debugf(fmt string, args ...interface{}) {
	common.Log.Debugf("[tracker] "+fmt, args...)
}

func (t *AWSVPCTracker) infof(fmt string, args ...interface{}) {
	common.Log.Infof("[tracker] "+fmt, args...)
}

// Helpers

// removeCommon filters out CIDR ranges which are contained in both a and b slices.
func removeCommon(a, b []address.CIDR) (newA, newB []address.CIDR) {
	i, j := 0, 0

	for i < len(a) && j < len(b) {
		switch {
		case a[i].Equal(b[j]):
			i++
			j++
			continue
		case a[i].End() < b[j].End():
			newA = append(newA, a[i])
			i++
		default:
			newB = append(newB, b[j])
			j++
		}
	}
	newA = append(newA, a[i:]...)
	newB = append(newB, b[j:]...)

	return
}

func parseCIDR(cidr string) (*net.IPNet, error) {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, err
	}
	ipnet.IP = ip

	return ipnet, nil
}