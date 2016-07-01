package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"runtime"
	//"time"

	"github.com/containernetworking/cni/pkg/ip"
	"github.com/containernetworking/cni/pkg/ipam"
	"github.com/containernetworking/cni/pkg/ns"
	"github.com/containernetworking/cni/pkg/skel"
	//"github.com/containernetworking/cni/pkg/types"
	//	"github.com/containernetworking/cni/pkg/utils"
	"github.com/vishvananda/netlink"
)

//const logFile = "/tmp/rancher-cni-network.log"
const logFile = "/tmp/rancher-cni.log"

const dockerBridgeName = "docker0"

func init() {
	// this ensures that main runs only on main thread (thread group leader).
	// since namespace ops (unshare, setns) are done for a single thread, we
	// must ensure that the goroutine does not jump from OS thread to thread
	runtime.LockOSThread()
}

func bridgeByName(name string) (*netlink.Bridge, error) {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return nil, fmt.Errorf("could not lookup %q: %v", name, err)
	}
	br, ok := l.(*netlink.Bridge)
	if !ok {
		return nil, fmt.Errorf("%q already exists but is not a bridge", name)
	}
	return br, nil
}

func setupVeth(netns ns.NetNS, br *netlink.Bridge, ifName string, mtu int, hairpinMode bool) error {

	log.Println("rancher-cni-network: %s", fmt.Sprintf("setupVeth: netns: %#v, br: %#v, ifName: %s, mtu: %d, hairpinMode: %t", netns, br, ifName, mtu, hairpinMode))

	var hostVethName string

	err := netns.Do(func(hostNS ns.NetNS) error {
		// create the veth pair in the container and move host end into host netns
		hostVeth, _, err := ip.SetupVeth(ifName, mtu, hostNS)
		if err != nil {
			return err
		}

		hostVethName = hostVeth.Attrs().Name
		return nil
	})
	if err != nil {
		return err
	}

	// need to lookup hostVeth again as its index has changed during ns move
	hostVeth, err := netlink.LinkByName(hostVethName)
	if err != nil {
		return fmt.Errorf("failed to lookup %q: %v", hostVethName, err)
	}

	// connect host veth end to the bridge
	if err = netlink.LinkSetMaster(hostVeth, br); err != nil {
		return fmt.Errorf("failed to connect %q to bridge %v: %v", hostVethName, br.Attrs().Name, err)
	}

	// set hairpin mode
	if err = netlink.LinkSetHairpin(hostVeth, hairpinMode); err != nil {
		return fmt.Errorf("failed to setup hairpin mode for %v: %v", hostVethName, err)
	}

	return nil
}

func teardownVeth(netns string, ifName string) error {

	log.Println("rancher-cni-network: %s", fmt.Sprintf("teardownVeth: netns: %s, ifName: %s", netns, ifName))

	// TODO: Check if it's needed to do LinkSetNoMaster first?

	var ipn *net.IPNet
	err := ns.WithNetNSPath(netns, func(_ ns.NetNS) error {
		var err error
		ipn, err = ip.DelLinkByNameAddr(ifName, netlink.FAMILY_V4)
		return err
	})
	if err != nil {
		return err
	}

	return nil
}

func cmdAdd(args *skel.CmdArgs) error {
	f, err := os.OpenFile(logFile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
	}
	defer f.Close()

	log.SetOutput(f)
	log.Println("rancher-cni-network: cmdAdd: invoked")
	log.Println("rancher-cni-network: %s", fmt.Sprintf("args: %#v", args))

	n, err := LoadNetConf(args.StdinData)
	if err != nil {
		return err
	}

	log.Println("rancher-cni-network: %s", fmt.Sprintf("n: %#v", n))

	// Make sure the "docker0" bridge exists
	br, err := bridgeByName(dockerBridgeName)
	if err != nil {
		return err
	}
	if br == nil {
		return errors.New(fmt.Sprintf("%s bridge is missing", dockerBridgeName))
	} else {
		log.Println("rancher-cni-network: %s", fmt.Sprintf("br:%#v", br))
	}

	if args.Netns == "" {
		return nil
	}

	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %v", args.Netns, err)
	}
	defer netns.Close()

	//if err = setupVeth(netns, br, args.IfName, n.MTU, n.HairpinMode); err != nil {
	if err = setupVeth(netns, br, args.IfName, 1400, false); err != nil {
		return err
	}

	// Call the IPAM and get the IP for the container
	log.Println("rancher-cni-network: %s", fmt.Sprintf("n.IPAM.Type: %#v", n.IPAM.Type))

	// run the IPAM plugin and get back the config to apply
	result, err := ipam.ExecAdd(n.IPAM.Type, args.StdinData)
	if err != nil {
		return err
	}

	log.Println("rancher-cni-network: %s", fmt.Sprintf("ipam result: %#v", result))
	log.Println("rancher-cni-network: %s", fmt.Sprintf("IP4: %#v", result.IP4))

	// TODO: make this optional when IPv6 is supported
	if result.IP4 == nil {
		return errors.New("IPAM plugin returned missing IPv4 config")
	}

	// Setup the IP address on the interface
	if err := netns.Do(func(_ ns.NetNS) error {
		return ipam.ConfigureIface(args.IfName, result)
	}); err != nil {
		return err
	}

	return result.Print()
}

func cmdDel(args *skel.CmdArgs) error {
	f, err := os.OpenFile(logFile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
	}
	defer f.Close()

	log.SetOutput(f)
	log.Println("rancher-cni-network: cmdDel: invoked")
	log.Println("rancher-cni-network: %s", fmt.Sprintf("args: %#v", args))

	n, err := LoadNetConf(args.StdinData)
	if err != nil {
		return err
	}

	if err := ipam.ExecDel(n.IPAM.Type, args.StdinData); err != nil {
		return err
	}

	if args.Netns == "" {
		return nil
	}

	teardownVeth(args.Netns, args.IfName)

	return nil
}

func main() {
	skel.PluginMain(cmdAdd, cmdDel)
}
