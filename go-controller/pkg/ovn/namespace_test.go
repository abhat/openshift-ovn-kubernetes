package ovn

import (
	"context"
	"net"

	"github.com/urfave/cli/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressfirewallfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned/fake"
	egressipfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/fake"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	apiextensionsfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func newNamespaceMeta(namespace string, additionalLabels map[string]string) metav1.ObjectMeta {
	labels := map[string]string{
		"name": namespace,
	}
	for k, v := range additionalLabels {
		labels[k] = v
	}
	return metav1.ObjectMeta{
		UID:         types.UID(namespace),
		Name:        namespace,
		Labels:      labels,
		Annotations: map[string]string{},
	}
}

func newNamespaceWithLabels(namespace string, additionalLabels map[string]string) *v1.Namespace {
	return &v1.Namespace{
		ObjectMeta: newNamespaceMeta(namespace, additionalLabels),
		Spec:       v1.NamespaceSpec{},
		Status:     v1.NamespaceStatus{},
	}
}

func newNamespace(namespace string) *v1.Namespace {
	return &v1.Namespace{
		ObjectMeta: newNamespaceMeta(namespace, nil),
		Spec:       v1.NamespaceSpec{},
		Status:     v1.NamespaceStatus{},
	}
}

var _ = Describe("OVN Namespace Operations", func() {
	const (
		namespaceName           = "namespace1"
		v4AddressSetName        = namespaceName + ipv4AddressSetSuffix
		v6AddressSetName        = namespaceName + ipv6AddressSetSuffix
		clusterIPNet     string = "10.1.0.0"
		clusterCIDR      string = clusterIPNet + "/16"
	)
	var (
		app     *cli.App
		fakeOvn *FakeOVN
	)

	BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOvn = NewFakeOVN(ovntest.NewFakeExec())
	})

	AfterEach(func() {
		fakeOvn.shutdown()
	})

	Context("on startup", func() {

		It("reconciles an existing namespace with pods", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace(namespaceName)
				tP := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"11:22:33:44:55:66",
					namespaceT.Name,
				)

				fakeOvn.start(ctx,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							*newPod(namespaceT.Name, tP.podName, tP.nodeName, tP.podIP),
						},
					},
				)
				podMAC := ovntest.MustParseMAC(tP.podMAC)
				podIPNets := []*net.IPNet{ovntest.MustParseIPNet(tP.podIP + "/24")}
				fakeOvn.controller.logicalPortCache.add(tP.nodeName, tP.portName, fakeUUID, podMAC, podIPNets)
				fakeOvn.controller.WatchNamespaces()

				_, err := fakeOvn.fakeClient.CoreV1().Namespaces().Get(context.TODO(), namespaceT.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())

				fakeOvn.asf.ExpectAddressSetWithIPs(v4AddressSetName, []string{tP.podIP})
				fakeOvn.asf.ExpectNoAddressSet(v6AddressSetName)

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("creates an empty address set for the namespace without pods", func() {
			app.Action = func(ctx *cli.Context) error {
				fakeOvn.start(ctx, &v1.NamespaceList{
					Items: []v1.Namespace{
						*newNamespace(namespaceName),
					},
				})
				fakeOvn.controller.WatchNamespaces()

				_, err := fakeOvn.fakeClient.CoreV1().Namespaces().Get(context.TODO(), namespaceName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())

				fakeOvn.asf.ExpectEmptyAddressSet(v4AddressSetName)
				fakeOvn.asf.ExpectNoAddressSet(v6AddressSetName)

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("creates an address set for existing nodes when the host network traffic namespace is created", func() {
			app.Action = func(ctx *cli.Context) error {
				node1 := tNode{
					Name:                 "node1",
					NodeIP:               "1.2.3.4",
					NodeLRPMAC:           "0a:58:0a:01:01:01",
					LrpMAC:               "0a:58:64:40:00:02",
					DrLrpMAC:             "0a:58:64:40:00:01",
					JoinSubnet:           "100.64.0.0/29",
					LrpIP:                "100.64.0.2",
					LrpIPv6:              "fd98::2",
					DrLrpIP:              "100.64.0.1",
					PhysicalBridgeMAC:    "11:22:33:44:55:66",
					SystemID:             "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6",
					TCPLBUUID:            "d2e858b2-cb5a-441b-a670-ed450f79a91f",
					UDPLBUUID:            "12832f14-eb0f-44d4-b8db-4cccbc73c792",
					SCTPLBUUID:           "0514c521-a120-4756-aec6-883fe5db7139",
					NodeSubnet:           "10.1.1.0/24",
					GWRouter:             util.GWRouterPrefix + "node1",
					GatewayRouterIPMask:  "172.16.16.2/24",
					GatewayRouterIP:      "172.16.16.2",
					GatewayRouterNextHop: "172.16.16.1",
					PhysicalBridgeName:   "br-eth0",
					NodeGWIP:             "10.1.1.1/24",
					NodeMgmtPortIP:       "10.1.1.2",
					NodeMgmtPortMAC:      "0a:58:0a:01:01:02",
					DnatSnatIP:           "169.254.0.1",
				}
				// create a test node and annotate it with host subnet
				testNode := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: node1.Name,
					},
					Status: kapi.NodeStatus{
						Addresses: []kapi.NodeAddress{
							{
								Type:    kapi.NodeExternalIP,
								Address: node1.NodeIP,
							},
						},
					},
				}

				hostNetworkNamespace := "test-host-network-ns"
				config.Kubernetes.HostNetworkNamespace = hostNetworkNamespace
				hostNetworkNs := *newNamespace(hostNetworkNamespace)

				fakeClient := fake.NewSimpleClientset(
					&v1.NamespaceList{
						Items: []v1.Namespace{
							hostNetworkNs,
						},
					},
				)
				egressFirewallFakeClient := &egressfirewallfake.Clientset{}
				egressIPFakeClient := &egressipfake.Clientset{}
				crdFakeClient := &apiextensionsfake.Clientset{}

				_, err := fakeClient.CoreV1().Nodes().Create(context.TODO(), &testNode, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{fakeClient, egressIPFakeClient, egressFirewallFakeClient}, &testNode)

				ifaceID := node1.PhysicalBridgeName + "_" + node1.Name
				vlanID := uint(1024)
				err = util.SetL3GatewayConfig(nodeAnnotator, &util.L3GatewayConfig{
					Mode:           config.GatewayModeShared,
					ChassisID:      node1.SystemID,
					InterfaceID:    ifaceID,
					MACAddress:     ovntest.MustParseMAC(node1.PhysicalBridgeMAC),
					IPAddresses:    ovntest.MustParseIPNets(node1.GatewayRouterIPMask),
					NextHops:       ovntest.MustParseIPs(node1.GatewayRouterNextHop),
					NodePortEnable: true,
					VLANID:         &vlanID,
				})
				err = util.SetNodeManagementPortMACAddress(nodeAnnotator, ovntest.MustParseMAC(node1.NodeMgmtPortMAC))
				Expect(err).NotTo(HaveOccurred())
				err = util.SetNodeHostSubnetAnnotation(nodeAnnotator, ovntest.MustParseIPNets(node1.NodeSubnet))
				Expect(err).NotTo(HaveOccurred())
				err = util.SetNodeJoinSubnetAnnotation(nodeAnnotator, ovntest.MustParseIPNets(node1.JoinSubnet))

				Expect(err).NotTo(HaveOccurred())
				err = util.SetNodeLocalNatAnnotation(nodeAnnotator, []net.IP{ovntest.MustParseIP(node1.DnatSnatIP)})
				Expect(err).NotTo(HaveOccurred())
				err = nodeAnnotator.Run()
				Expect(err).NotTo(HaveOccurred())

				updatedNode, err := fakeClient.CoreV1().Nodes().Get(context.TODO(), node1.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				nodeHostSubnetAnnotations, err := util.ParseNodeHostSubnetAnnotation(updatedNode)
				Expect(err).NotTo(HaveOccurred())
				Eventually(nodeHostSubnetAnnotations[0].String()).Should(Equal(node1.NodeSubnet))
				_, err = config.InitConfig(ctx, fakeOvn.fakeExec, nil)
				Expect(err).NotTo(HaveOccurred())
				fakeOvn.fakeClient = fakeClient
				fakeOvn.fakeEgressIPClient = egressIPFakeClient
				fakeOvn.fakeEgressClient = egressFirewallFakeClient
				fakeOvn.fakeCRDClient = crdFakeClient
				fakeOvn.init()

				fakeOvn.controller.multicastSupport = false
				fakeOvn.controller.TCPLoadBalancerUUID = node1.TCPLBUUID
				fakeOvn.controller.UDPLoadBalancerUUID = node1.UDPLBUUID
				fakeOvn.controller.SCTPLoadBalancerUUID = node1.SCTPLBUUID

				_, clusterNetwork, err := net.ParseCIDR(clusterCIDR)
				Expect(err).NotTo(HaveOccurred())

				fakeOvn.controller.masterSubnetAllocator.AddNetworkRange(clusterNetwork, 24)

				fakeOvn.controller.SCTPSupport = true

				fexec := fakeOvn.fakeExec
				addNodeLogicalFlows(fexec, &node1, clusterCIDR, config.IPv6Mode, false)

				fakeOvn.controller.WatchNamespaces()
				hostnsAddrSet4 := hostNetworkNamespace + "_v4"
				fakeOvn.asf.EventuallyExpectEmptyAddressSet(hostnsAddrSet4)
				fakeOvn.controller.WatchNodes()

				Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)

				// check the namespace again and ensure the address set
				// being created with the right set of IPs in it.
				allowIPs := []string{node1.NodeMgmtPortIP}
				fakeOvn.asf.ExpectAddressSetWithIPs(hostnsAddrSet4, allowIPs)

				return nil
			}

			err := app.Run([]string{
				app.Name,
				"-cluster-subnets=" + clusterCIDR,
				"--init-gateways",
				"--nodeport",
			})
			Expect(err).NotTo(HaveOccurred())

		})
	})

	Context("during execution", func() {
		It("deletes an empty namespace's resources", func() {
			app.Action = func(ctx *cli.Context) error {
				fakeOvn.start(ctx, &v1.NamespaceList{
					Items: []v1.Namespace{
						*newNamespace(namespaceName),
					},
				})
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.asf.ExpectEmptyAddressSet(v4AddressSetName)
				fakeOvn.asf.ExpectNoAddressSet(v6AddressSetName)

				err := fakeOvn.fakeClient.CoreV1().Namespaces().Delete(context.TODO(), namespaceName, *metav1.NewDeleteOptions(1))
				Expect(err).NotTo(HaveOccurred())
				fakeOvn.asf.EventuallyExpectNoAddressSet(v4AddressSetName)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
