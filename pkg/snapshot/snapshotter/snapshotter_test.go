package snapshotter_test

import (
	"path"
	"time"

	. "github.com/gardener/etcd-backup-restore/pkg/snapshot/snapshotter"
	"github.com/gardener/etcd-backup-restore/pkg/snapstore"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/sirupsen/logrus"
)

var _ = Describe("Snapshotter", func() {
	var (
		endpoints             []string
		store                 snapstore.SnapStore
		logger                *logrus.Logger
		etcdConnectionTimeout time.Duration
		schedule              string
		certFile              string
		keyFile               string
		caFile                string
		insecureTransport     bool
		insecureSkipVerify    bool
		err                   error
	)
	BeforeEach(func() {
		endpoints = []string{"http://localhost:2379"}
		logger = logrus.New()
		etcdConnectionTimeout = 10
		schedule = "*/1 * * * *"
	})

	Describe("creating Snapshotter", func() {
		var ssr *Snapshotter
		BeforeEach(func() {
			store, err = snapstore.GetSnapstore(&snapstore.Config{Container: path.Join(outputDir, "snapshotter_1.bkp")})
			Expect(err).ShouldNot(HaveOccurred())
		})
		Context("With invalid schedule", func() {
			It("should return error", func() {
				schedule = "65 * * * 5"
				ssr, err = NewSnapshotter(endpoints, schedule, store, logger, 1, etcdConnectionTimeout, certFile, keyFile, caFile, insecureTransport, insecureSkipVerify)
				Expect(err).Should(HaveOccurred())
				Expect(ssr).Should(BeNil())
			})
		})

		Context("With valid schedule", func() {
			It("should create snapshotter", func() {
				schedule = "*/5 * * * *"
				ssr, err = NewSnapshotter(endpoints, schedule, store, logger, 1, etcdConnectionTimeout, certFile, keyFile, caFile, insecureTransport, insecureSkipVerify)
				Expect(err).ShouldNot(HaveOccurred())
				Expect(ssr).ShouldNot(BeNil())
			})
		})
	})

	Describe("running snapshotter", func() {
		Context("with etcd not running at configured endpoint", func() {
			It("should timeout & not take any snapshot", func() {
				stopCh := make(chan struct{})
				endpoints = []string{"http://localhost:5000"}
				etcdConnectionTimeout = 5
				maxBackups := 2
				testTimeout := time.Duration(time.Minute * time.Duration(maxBackups+1))
				store, err = snapstore.GetSnapstore(&snapstore.Config{Container: path.Join(outputDir, "snapshotter_2.bkp")})
				Expect(err).ShouldNot(HaveOccurred())
				ssr, err := NewSnapshotter(endpoints, schedule, store, logger, maxBackups, etcdConnectionTimeout, certFile, keyFile, caFile, insecureTransport, insecureSkipVerify)
				Expect(err).ShouldNot(HaveOccurred())
				go func() {
					<-time.After(testTimeout)
					close(stopCh)
				}()
				err = ssr.Run(stopCh)
				Expect(err).Should(HaveOccurred())
				list, err := store.List()
				Expect(err).ShouldNot(HaveOccurred())
				Expect(len(list)).Should(BeZero())
			})
		})
		Context("with etcd running at configured endpoint", func() {
			BeforeEach(func() {
				endpoints = []string{"http://localhost:2379"}
			})
			Context("with unreachable schedule", func() {
				var ssr *Snapshotter
				BeforeEach(func() {
					stopCh := make(chan struct{})
					schedule = "* * 31 2 *"
					etcdConnectionTimeout = 5
					maxBackups := 2
					testTimeout := time.Duration(time.Minute * time.Duration(maxBackups+1))
					store, err = snapstore.GetSnapstore(&snapstore.Config{Container: path.Join(outputDir, "snapshotter_3.bkp")})
					Expect(err).ShouldNot(HaveOccurred())
					ssr, err = NewSnapshotter(endpoints, schedule, store, logger, maxBackups, etcdConnectionTimeout, certFile, keyFile, caFile, insecureTransport, insecureSkipVerify)
					Expect(err).ShouldNot(HaveOccurred())
					go func() {
						<-time.After(testTimeout)
						close(stopCh)
					}()
					err = ssr.Run(stopCh)
				})
				It("should return immediately without errorand any snapshot", func() {
					Expect(err).ShouldNot(HaveOccurred())
				})
				It("should not take any snapshot", func() {
					list, err := store.List()
					Expect(err).ShouldNot(HaveOccurred())
					Expect(len(list)).Should(BeZero())
				})
			})
			Context("with valid schedule", func() {
				var (
					ssr        *Snapshotter
					maxBackups int
				)
				It("take periodic backups and garbage collect backups over maxBackups configured", func() {
					stopCh := make(chan struct{})
					endpoints = []string{"http://localhost:2379"}
					//We will wait for maxBackups+1 times schedule period
					schedule = "*/1 * * * *"
					maxBackups = 2
					testTimeout := time.Duration(time.Minute * time.Duration(maxBackups+1))
					etcdConnectionTimeout = 5
					store, err = snapstore.GetSnapstore(&snapstore.Config{Container: path.Join(outputDir, "snapshotter_4.bkp")})
					Expect(err).ShouldNot(HaveOccurred())
					ssr, err = NewSnapshotter(endpoints, schedule, store, logger, maxBackups, etcdConnectionTimeout, certFile, keyFile, caFile, insecureTransport, insecureSkipVerify)
					Expect(err).ShouldNot(HaveOccurred())
					go func() {
						<-time.After(testTimeout)
						close(stopCh)
					}()
					err = ssr.Run(stopCh)
					Expect(err).ShouldNot(HaveOccurred())
					list, err := store.List()
					Expect(err).ShouldNot(HaveOccurred())
					Expect(len(list)).ShouldNot(BeZero())

				})
			})
		})
	})
})