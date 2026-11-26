// Copyright 2016-2018 CNI authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ns_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"golang.org/x/sys/unix"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
)

func getInodeCurNetNS() (uint64, error) {
	curNS, err := ns.GetCurrentNS()
	if err != nil {
		return 0, err
	}
	defer curNS.Close()
	return getInodeNS(curNS)
}

func getInodeNS(netns ns.NetNS) (uint64, error) {
	return getInodeFd(int(netns.Fd()))
}

func getInode(path string) (uint64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	return getInodeFd(int(file.Fd()))
}

func getInodeFd(fd int) (uint64, error) {
	stat := &unix.Stat_t{}
	err := unix.Fstat(fd, stat)
	return stat.Ino, err
}

var _ = Describe("Linux namespace operations", func() {
	Describe("WithNetNS", func() {
		var (
			originalNetNS ns.NetNS
			targetNetNS   ns.NetNS
		)

		BeforeEach(func() {
			var err error

			originalNetNS, err = testutils.NewNS()
			Expect(err).NotTo(HaveOccurred())

			targetNetNS, err = testutils.NewNS()
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			targetNetNS.Close()
			originalNetNS.Close()

			Expect(testutils.UnmountNS(targetNetNS)).To(Succeed())
			Expect(testutils.UnmountNS(originalNetNS)).To(Succeed())
		})

		It("executes the callback within the target network namespace", func() {
			expectedInode, err := getInodeNS(targetNetNS)
			Expect(err).NotTo(HaveOccurred())

			err = targetNetNS.Do(func(ns.NetNS) error {
				defer GinkgoRecover()

				actualInode, err := getInodeCurNetNS()
				Expect(err).NotTo(HaveOccurred())
				Expect(actualInode).To(Equal(expectedInode))
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("provides the original namespace as the argument to the callback", func() {
			// Ensure we start in originalNetNS
			err := originalNetNS.Do(func(ns.NetNS) error {
				defer GinkgoRecover()

				origNSInode, err := getInodeNS(originalNetNS)
				Expect(err).NotTo(HaveOccurred())

				err = targetNetNS.Do(func(hostNS ns.NetNS) error {
					defer GinkgoRecover()

					hostNSInode, err := getInodeNS(hostNS)
					Expect(err).NotTo(HaveOccurred())
					Expect(hostNSInode).To(Equal(origNSInode))
					return nil
				})
				Expect(err).NotTo(HaveOccurred())
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		Context("when called concurrently", func() {
			It("provides the original namespace as the argument to the callback", func() {
				concurrency := 200
				origNS, err := ns.GetCurrentNS()
				Expect(err).NotTo(HaveOccurred())
				origNSInode, err := getInodeNS(origNS)
				Expect(err).NotTo(HaveOccurred())

				var wg sync.WaitGroup
				wg.Add(concurrency)
				for i := 0; i < concurrency; i++ {
					go func() {
						defer wg.Done()
						targetNetNS.Do(func(hostNS ns.NetNS) error {
							defer GinkgoRecover()

							hostNSInode, err := getInodeNS(hostNS)
							Expect(err).NotTo(HaveOccurred())
							Expect(hostNSInode).To(Equal(origNSInode))
							return nil
						})
					}()
				}
				wg.Wait()
			})
		})

		Context("when the callback returns an error", func() {
			It("restores the calling thread to the original namespace before returning", func() {
				err := originalNetNS.Do(func(ns.NetNS) error {
					defer GinkgoRecover()

					preTestInode, err := getInodeCurNetNS()
					Expect(err).NotTo(HaveOccurred())

					_ = targetNetNS.Do(func(ns.NetNS) error {
						return errors.New("potato")
					})

					postTestInode, err := getInodeCurNetNS()
					Expect(err).NotTo(HaveOccurred())
					Expect(postTestInode).To(Equal(preTestInode))
					return nil
				})
				Expect(err).NotTo(HaveOccurred())
			})

			It("returns the error from the callback", func() {
				err := targetNetNS.Do(func(ns.NetNS) error {
					return errors.New("potato")
				})
				Expect(err).To(MatchError("potato"))
			})
		})

		Describe("validating inode mapping to namespaces", func() {
			It("checks that different namespaces have different inodes", func() {
				origNSInode, err := getInodeNS(originalNetNS)
				Expect(err).NotTo(HaveOccurred())

				testNsInode, err := getInodeNS(targetNetNS)
				Expect(err).NotTo(HaveOccurred())

				Expect(testNsInode).NotTo(Equal(uint64(0)))
				Expect(testNsInode).NotTo(Equal(origNSInode))
			})

			It("should not leak a closed netns onto any threads in the process", func() {
				By("creating a new netns")
				createdNetNS, err := testutils.NewNS()
				Expect(err).NotTo(HaveOccurred())

				By("discovering the inode of the created netns")
				createdNetNSInode, err := getInodeNS(createdNetNS)
				Expect(err).NotTo(HaveOccurred())
				createdNetNS.Close()
				Expect(testutils.UnmountNS(createdNetNS)).NotTo(HaveOccurred())

				By("comparing against the netns inode of every thread in the process")
				for _, netnsPath := range allNetNSInCurrentProcess() {
					netnsInode, err := getInode(netnsPath)
					if !os.IsNotExist(err) {
						Expect(err).NotTo(HaveOccurred())
					}
					Expect(netnsInode).NotTo(Equal(createdNetNSInode))
				}
			})

			It("fails when the path is not a namespace", func() {
				tempFile, err := os.CreateTemp("", "nstest")
				Expect(err).NotTo(HaveOccurred())
				defer tempFile.Close()

				nspath := tempFile.Name()
				defer os.Remove(nspath)

				_, err = ns.GetNS(nspath)
				Expect(err).To(HaveOccurred())
				Expect(err).To(BeAssignableToTypeOf(ns.NSPathNotNSErr{}))
				Expect(err).NotTo(BeAssignableToTypeOf(ns.NSPathNotExistErr{}))
			})
		})

		Describe("closing a network namespace", func() {
			It("should prevent further operations", func() {
				createdNetNS, err := testutils.NewNS()
				defer testutils.UnmountNS(createdNetNS)
				Expect(err).NotTo(HaveOccurred())

				err = createdNetNS.Close()
				Expect(err).NotTo(HaveOccurred())

				err = createdNetNS.Do(func(ns.NetNS) error { return nil })
				Expect(err).To(HaveOccurred())

				err = createdNetNS.Set()
				Expect(err).To(HaveOccurred())
			})

			It("should only work once", func() {
				createdNetNS, err := testutils.NewNS()
				Expect(err).NotTo(HaveOccurred())
				defer testutils.UnmountNS(createdNetNS)

				err = createdNetNS.Close()
				Expect(err).NotTo(HaveOccurred())

				// Repeated closes are idempotent and never error so
				// callers can safely defer them on shared handles.
				err = createdNetNS.Close()
				Expect(err).NotTo(HaveOccurred())
				err = createdNetNS.Close()
				Expect(err).NotTo(HaveOccurred())
			})

			It("should be safe to close concurrently from many goroutines", func() {
				createdNetNS, err := testutils.NewNS()
				Expect(err).NotTo(HaveOccurred())
				defer testutils.UnmountNS(createdNetNS)

				closers := 50
				var wg sync.WaitGroup
				wg.Add(closers)
				errCh := make(chan error, closers)
				for i := 0; i < closers; i++ {
					go func() {
						defer wg.Done()
						errCh <- createdNetNS.Close()
					}()
				}
				wg.Wait()
				close(errCh)

				errorCount := 0
				for closeErr := range errCh {
					if closeErr != nil {
						errorCount++
					}
				}
				Expect(errorCount).To(BeZero())

				// A closed handle must refuse further namespace operations.
				err = createdNetNS.Set()
				Expect(err).To(HaveOccurred())
				err = createdNetNS.Do(func(ns.NetNS) error { return nil })
				Expect(err).To(HaveOccurred())
			})
		})

		Describe("restoring the namespace under failures and panics", func() {
			It("recovers a panicking callback, reports it and restores the namespace", func() {
				preTestInode, err := getInodeCurNetNS()
				Expect(err).NotTo(HaveOccurred())

				err = targetNetNS.Do(func(ns.NetNS) error {
					panic("potato")
				})
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("panic in netns callback"))
				Expect(err.Error()).To(ContainSubstring("potato"))

				// The calling goroutine's thread must be back in the
				// original namespace, not stranded in the target one.
				postTestInode, err := getInodeCurNetNS()
				Expect(err).NotTo(HaveOccurred())
				Expect(postTestInode).To(Equal(preTestInode))
			})

			It("does not crash when callbacks panic concurrently", func() {
				concurrency := 100
				var wg sync.WaitGroup
				wg.Add(concurrency)
				for i := 0; i < concurrency; i++ {
					go func(i int) {
						defer wg.Done()
						defer GinkgoRecover()

						err := targetNetNS.Do(func(ns.NetNS) error {
							if i%2 == 0 {
								panic("concurrent potato")
							}
							return errors.New("plain error")
						})
						Expect(err).To(HaveOccurred())
					}(i)
				}
				wg.Wait()
			})

			It("keeps switching and restoring correctly under heavy concurrency", func() {
				origInode, err := getInodeCurNetNS()
				Expect(err).NotTo(HaveOccurred())
				targetInode, err := getInodeNS(targetNetNS)
				Expect(err).NotTo(HaveOccurred())

				concurrency := 200
				var wg sync.WaitGroup
				wg.Add(concurrency)
				for i := 0; i < concurrency; i++ {
					go func() {
						defer wg.Done()
						defer GinkgoRecover()

						err := targetNetNS.Do(func(ns.NetNS) error {
							inode, err := getInodeCurNetNS()
							Expect(err).NotTo(HaveOccurred())
							Expect(inode).To(Equal(targetInode))
							return nil
						})
						Expect(err).NotTo(HaveOccurred())

						// Every Do must leave the caller back in its
						// original namespace.
						inode, err := getInodeCurNetNS()
						Expect(err).NotTo(HaveOccurred())
						Expect(inode).To(Equal(origInode))
					}()
				}
				wg.Wait()
			})

			It("supports nested Do calls without deadlocking", func() {
				targetInode, err := getInodeNS(targetNetNS)
				Expect(err).NotTo(HaveOccurred())

				err = targetNetNS.Do(func(ns.NetNS) error {
					defer GinkgoRecover()

					return originalNetNS.Do(func(hostNS ns.NetNS) error {
						defer GinkgoRecover()

						originalInode, err := getInodeNS(originalNetNS)
						Expect(err).NotTo(HaveOccurred())
						current, err := getInodeCurNetNS()
						Expect(err).NotTo(HaveOccurred())
						Expect(current).To(Equal(originalInode))
						// hostNS is the namespace the outer Do switched
						// away from, i.e. the target namespace.
						hostInode, err := getInodeNS(hostNS)
						Expect(err).NotTo(HaveOccurred())
						Expect(hostInode).To(Equal(targetInode))
						return nil
					})
				})
				Expect(err).NotTo(HaveOccurred())
			})
		})

		Describe("entering a namespace that does not exist", func() {
			It("fails before touching any namespace state", func() {
				beforeInode, err := getInodeCurNetNS()
				Expect(err).NotTo(HaveOccurred())

				missingPath := "/var/run/netns/this-ns-does-not-exist"
				err = ns.WithNetNSPath(missingPath, func(ns.NetNS) error {
					Fail("callback must not run for a missing namespace")
					return nil
				})
				Expect(err).To(HaveOccurred())
				Expect(err).To(BeAssignableToTypeOf(ns.NSPathNotExistErr{}))

				afterInode, err := getInodeCurNetNS()
				Expect(err).NotTo(HaveOccurred())
				Expect(afterInode).To(Equal(beforeInode))
			})

			It("fails on a path that is not a namespace", func() {
				tempFile, err := os.CreateTemp("", "nstest")
				Expect(err).NotTo(HaveOccurred())
				defer tempFile.Close()

				nspath := tempFile.Name()
				defer os.Remove(nspath)

				err = ns.WithNetNSPath(nspath, func(ns.NetNS) error {
					Fail("callback must not run for a non-namespace path")
					return nil
				})
				Expect(err).To(HaveOccurred())
				Expect(err).To(BeAssignableToTypeOf(ns.NSPathNotNSErr{}))
			})
		})
	})

	Describe("IsNSorErr", func() {
		It("should detect a namespace", func() {
			createdNetNS, err := testutils.NewNS()
			Expect(err).NotTo(HaveOccurred())
			defer testutils.UnmountNS(createdNetNS)
			err = ns.IsNSorErr(createdNetNS.Path())
			Expect(err).NotTo(HaveOccurred())
		})

		It("should refuse other paths", func() {
			tempFile, err := os.CreateTemp("", "nstest")
			Expect(err).NotTo(HaveOccurred())
			defer tempFile.Close()

			nspath := tempFile.Name()
			defer os.Remove(nspath)

			err = ns.IsNSorErr(nspath)
			Expect(err).To(HaveOccurred())
			Expect(err).To(BeAssignableToTypeOf(ns.NSPathNotNSErr{}))
			Expect(err).NotTo(BeAssignableToTypeOf(ns.NSPathNotExistErr{}))
		})

		It("should error on non-existing paths", func() {
			err := ns.IsNSorErr("/tmp/IDoNotExist")
			Expect(err).To(HaveOccurred())
			Expect(err).To(BeAssignableToTypeOf(ns.NSPathNotExistErr{}))
			Expect(err).NotTo(BeAssignableToTypeOf(ns.NSPathNotNSErr{}))
		})
	})
})

func allNetNSInCurrentProcess() []string {
	pid := unix.Getpid()
	paths, err := filepath.Glob(fmt.Sprintf("/proc/%d/task/*/ns/net", pid))
	Expect(err).NotTo(HaveOccurred())
	return paths
}
