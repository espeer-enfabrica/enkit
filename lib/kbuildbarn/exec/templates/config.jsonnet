// List of clusters to which bb_clientd is permitted to connect.
local clusters = [
  '{{.BuildbarnHost}}',
];

local grpcClient(cluster) = {
  address: cluster + ':{{.BuildbarnPort}}',
  forwardMetadata: ['build.bazel.remote.execution.v2.requestmetadata-bin'],
  // Enable gRPC keepalives. Make sure to tune these settings based on
  // what your cluster permits.
  keepalive: {
    time: '60s',
    timeout: '30s',
  },
};

// Route requests to one of the clusters listed above by parsing the
// prefix of the instance name. This prefix will be stripped on outgoing
// requests.
local blobstoreConfig = {
  demultiplexing: {
    instanceNamePrefixes: {
      [cluster]: { backend: { grpc: grpcClient(cluster) } }
      for cluster in clusters
    },
  },
};

{
  // Maximum supported Protobuf message size.
  maximumMessageSizeBytes: 16 * 1024 * 1024,
    casKeyLocationMapSizeBytes:: 512 * 1024 * 1024,
    casBlocksSizeBytes:: 100 * 1024 * 1024 * 1024,
    filePoolSizeBytes:: 100 * 1024 * 1024 * 1024,

  // Backends for the Action Cache and Content Addressable Storage.
  blobstore: {
    actionCache: blobstoreConfig,
    contentAddressableStorage: {
      readCaching: {
        // Clusters are the source of truth.
        slow: {
          existenceCaching: {
            backend: blobstoreConfig,
            // Assume that if FindMissingBlobs() reports a blob as being
            // present, it's going to stay around for five more minutes.
            // This significantly reduces the combined size of
            // FindMissingBlobs() calls generated by Bazel.
            existenceCache: {
              cacheSize: 1000 * 1000,
              cacheDuration: '300s',
              cacheReplacementPolicy: 'LEAST_RECENTLY_USED',
            },
          },
        },
        // On-disk cache to speed up access to recently used objects.
        // Let the cache consume up to 100 GiB of disk space. A 64 MiB
        // index is large enough to accomodate approximately one million
        // objects.
        fast: {
          'local': {
            keyLocationMapOnBlockDevice: {
              file: {
                path: '{{.KeyLocationMapPath}}',
                sizeBytes: $.casKeyLocationMapSizeBytes,
              },
            },
            keyLocationMapMaximumGetAttempts: 8,
            keyLocationMapMaximumPutAttempts: 32,
            oldBlocks: 1,
            currentBlocks: 5,
            newBlocks: 1,
            blocksOnBlockDevice: {
              source: {
                file: {
                  path: '{{.CasBlocksDir}}',
                  sizeBytes: $.casBlocksSizeBytes,
                },
              },
              spareBlocks: 1,
              dataIntegrityValidationCache: {
                cacheSize: 100000,
                cacheDuration: '14400s',
                cacheReplacementPolicy: 'LEAST_RECENTLY_USED',
              },
            },
            persistent: {
              stateDirectoryPath: '{{.PersistentStateDir}}',
              minimumEpochInterval: '300s',
            },
          },
        },
        replicator: {
          deduplicating: {
            // Bazel's -j flag not only affects the number of actions
            // executed concurrently. It also influences the concurrency
            // of ByteStream requests. Prevent starvation by limiting
            // the number of requests that are forwarded when cache
            // misses occur.
            concurrencyLimiting: {
              base: { 'local': {} },
              maximumConcurrency: 100,
            },
          },
        },
      },
    },
  },

  // Schedulers to which to route execution requests. This uses the same
  // routing policy as the storage configuration above.
  schedulers: {
    [cluster]: { endpoint: grpcClient(cluster) }
    for cluster in clusters
  },

  // A gRPC server to which Bazel can send requests, as opposed to
  // contacting clusters directly. This allows bb_clientd to capture
  // credentials.
  grpcServers: [{
    listenPaths: ['{{.GRPCSocketPath}}'],
    authenticationPolicy: { allow: {} },
  }],

  // The FUSE file system through which data stored in the Content
  // Addressable Storage can be loaded lazily. This file system relies
  // on credentials captured through gRPC.
  fuse: {
    mountPath: '{{.MountDir}}',
    directoryEntryValidity: '300s',
    inodeAttributeValidity: '300s',
    // Enabling this option may be necessary if you want to permit
    // super-user access to the FUSE file system. It is strongly
    // recommended that the permissions on the parent directory of the
    // FUSE file system are locked down before enabling this option.
    // allowOther: true,
  },

  // The location where locally created files in the "scratch" and
  // "outputs" directories of the FUSE file system are stored. These
  // files are not necessarily backed by remote storage.
  filePool: { blockDevice: { file: {
    path: '{{.FilePoolDir}}',
    sizeBytes: $.filePoolSizeBytes,
  } } },

  // The location where contents of the "outputs" are stored, so that
  // they may be restored after restarts of bb_clientd. Because data is
  // stored densely, and only the metadata of files is stored (i.e.,
  // REv2 digests), these files tend to be small.
  outputPathPersistency: {
    stateDirectoryPath: '{{.OutputsDir}}',
    maximumStateFileSizeBytes: 1024 * 1024 * 1024,
    maximumStateFileAge: '604800s',
  },

  global: {
    // Multiplex logs into a file. That way they remain accessible, even
    // if bb_clientd is run through a system that doesn't maintain logs
    // for us.
    logPaths: ['{{.LogsDir}}'],

    // Attach credentials provided by Bazel to all outgoing gRPC calls.
    grpcForwardAndReuseMetadata: ['authorization'],

    // Optional: create a HTTP server that exposes Prometheus metrics
    // and allows debugging using pprof. Make sure to only enable it
    // when you need it, or at least make sure that access is limited.
    /*
    diagnosticsHttpServer: {
      listenAddress: '127.0.0.1:12345',
      enablePrometheus: true,
      enablePprof: true,
      enableActiveSpans: true,
    },
    */
  },
}