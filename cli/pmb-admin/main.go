package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/signal"
	"sort"

	"text/template"

	"github.com/alecthomas/kingpin"
	"github.com/percona/mongodb-backup/internal/templates"
	pbapi "github.com/percona/mongodb-backup/proto/api"
	pb "github.com/percona/mongodb-backup/proto/messages"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/testdata"
	yaml "gopkg.in/yaml.v2"
)

type cliOptions struct {
	app *kingpin.Application

	TLS        bool   `yaml:"tls"`
	CAFile     string `yaml:"ca_file"`
	ServerAddr string `yaml:"server_addr"`
	configFile *string

	run *kingpin.CmdClause

	backup               *kingpin.CmdClause
	backupType           *string
	destinationType      *string
	compressionAlgorithm *string
	encryptionAlgorithm  *string
	description          *string

	restore                  *kingpin.CmdClause
	restoreMetadataFile      *string
	restoreSkipUsersAndRoles *bool

	list             *kingpin.CmdClause
	listNodes        *kingpin.CmdClause
	listNodesVerbose *bool
	listBackups      *kingpin.CmdClause
}

var (
	conn              *grpc.ClientConn
	defaultServerAddr = "127.0.0.1:10001"
	defaultConfigFile = "~/.pmb-admin.yml"
)

func main() {
	cmd, opts, err := processCliArgs(os.Args[1:])
	if err != nil {
		if opts != nil {
			kingpin.Usage()
		}
		log.Fatal(err)
	}

	var grpcOpts []grpc.DialOption
	if opts.TLS {
		if opts.CAFile == "" {
			opts.CAFile = testdata.Path("ca.pem")
		}
		creds, err := credentials.NewClientTLSFromFile(opts.CAFile, "")
		if err != nil {
			log.Fatalf("Failed to create TLS credentials %v", err)
		}
		grpcOpts = append(grpcOpts, grpc.WithTransportCredentials(creds))
	} else {
		grpcOpts = append(grpcOpts, grpc.WithInsecure())
	}

	ctx, cancel := context.WithCancel(context.Background())

	conn, err = grpc.Dial(opts.ServerAddr, grpcOpts...)
	if err != nil {
		log.Fatalf("fail to dial: %v", err)
	}
	defer conn.Close()

	apiClient := pbapi.NewApiClient(conn)
	if err != nil {
		log.Fatalf("Cannot connect to the API: %s", err)
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	go func() {
		<-c
		cancel()
	}()

	switch cmd {
	case "list nodes":
		clients, err := connectedAgents(ctx, conn)
		if err != nil {
			log.Errorf("Cannot get the list of connected agents: %s", err)
			break
		}
		if *opts.listNodesVerbose {
			printTemplate(templates.ConnectedNodesVerbose, clients)
		} else {
			printTemplate(templates.ConnectedNodes, clients)
		}
	case "list backups":
		md, err := getAvailableBackups(ctx, conn)
		if err != nil {
			log.Errorf("Cannot get the list of available backups: %s", err)
			break
		}
		if len(md) > 0 {
			printTemplate(templates.AvailableBackups, md)
			return
		}
		fmt.Println("No backups found")
	case "run backup":
		err := startBackup(ctx, apiClient, opts)
		if err != nil {
			log.Fatal(err)
			log.Fatalf("Cannot send the StartBackup command to the gRPC server: %s", err)
		}
	case "run restore":
		fmt.Println("restoring")
		err := restoreBackup(ctx, apiClient, opts)
		if err != nil {
			log.Fatal(err)
			log.Fatalf("Cannot send the RestoreBackup command to the gRPC server: %s", err)
		}
	default:
		log.Fatalf("Unknown command %q", cmd)
	}

	cancel()
}

func connectedAgents(ctx context.Context, conn *grpc.ClientConn) ([]*pbapi.Client, error) {
	apiClient := pbapi.NewApiClient(conn)
	stream, err := apiClient.GetClients(ctx, &pbapi.Empty{})
	if err != nil {
		return nil, err
	}
	clients := []*pbapi.Client{}
	for {
		msg, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, errors.Wrap(err, "Cannot get the connected agents list")
		}
		clients = append(clients, msg)
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i].NodeName < clients[j].NodeName })
	return clients, nil
}

func getAvailableBackups(ctx context.Context, conn *grpc.ClientConn) (map[string]*pb.BackupMetadata, error) {
	apiClient := pbapi.NewApiClient(conn)
	stream, err := apiClient.BackupsMetadata(ctx, &pbapi.BackupsMetadataParams{})
	if err != nil {
		return nil, err
	}

	mds := make(map[string]*pb.BackupMetadata)
	for {
		msg, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, errors.Wrap(err, "Cannot get the connected agents list")
		}
		mds[msg.Filename] = msg.Metadata
	}

	return mds, nil
}

// This function is used by autocompletion. Currently, when it is called, the gRPC connection is nil
// because command line parameters havent been processed yet.
// Maybe in the future, we could read the defaults from a config file. For now, just try to connect
// to a server running on the local host
func listAvailableBackups() (backups []string) {
	var err error
	if conn == nil {
		conn, err = grpc.Dial(defaultServerAddr, []grpc.DialOption{grpc.WithInsecure()}...)
		if err != nil {
			return
		}
		defer conn.Close()
	}

	mds, err := getAvailableBackups(context.TODO(), conn)
	if err != nil {
		return
	}

	for name, md := range mds {
		backup := fmt.Sprintf("%s -> %s", name, md.Description)
		backups = append(backups, backup)
	}
	return
}

func printTemplate(tpl string, data interface{}) {
	var b bytes.Buffer
	tmpl := template.Must(template.New("").Parse(tpl))
	if err := tmpl.Execute(&b, data); err != nil {
		log.Fatal(err)
	}
	print(b.String())
}

func startBackup(ctx context.Context, apiClient pbapi.ApiClient, opts *cliOptions) error {
	msg := &pbapi.RunBackupParams{
		CompressionType: pbapi.CompressionType_COMPRESSION_TYPE_NO_COMPRESSION,
		Cypher:          pbapi.Cypher_CYPHER_NO_CYPHER,
		Description:     *opts.description,
	}

	switch *opts.backupType {
	case "logical":
		msg.BackupType = pbapi.BackupType_BACKUP_TYPE_LOGICAL
	case "hot":
		msg.BackupType = pbapi.BackupType_BACKUP_TYPE_HOTBACKUP
	}

	switch *opts.destinationType {
	case "logical":
		msg.DestinationType = pbapi.DestinationType_DESTINATION_TYPE_FILE
	case "aws":
		msg.DestinationType = pbapi.DestinationType_DESTINATION_TYPE_AWS
	}

	switch *opts.compressionAlgorithm {
	case "gzip":
		msg.CompressionType = pbapi.CompressionType_COMPRESSION_TYPE_GZIP
	}

	switch opts.encryptionAlgorithm {
	}

	_, err := apiClient.RunBackup(ctx, msg)
	if err != nil {
		return err
	}

	return nil
}

func restoreBackup(ctx context.Context, apiClient pbapi.ApiClient, opts *cliOptions) error {
	msg := &pbapi.RunRestoreParams{
		MetadataFile:      *opts.restoreMetadataFile,
		SkipUsersAndRoles: *opts.restoreSkipUsersAndRoles,
	}

	_, err := apiClient.RunRestore(ctx, msg)
	if err != nil {
		return err
	}

	return nil
}

func processCliArgs(args []string) (string, *cliOptions, error) {
	app := kingpin.New("mongodb-backup-admin", "MongoDB backup admin")

	runCmd := app.Command("run", "Start a new backup or restore process")

	getCmd := app.Command("list", "List objects (connected nodes, backups, etc)")
	getBackupsCmd := getCmd.Command("backups", "List backups")
	getNodesCmd := getCmd.Command("nodes", "List objects (connected nodes, backups, etc)")

	backupCmd := runCmd.Command("backup", "Start a backup")
	restoreCmd := runCmd.Command("restore", "Restore a backup given a metadata file name")

	opts := &cliOptions{
		configFile: app.Flag("config", "Config file name").Default(defaultConfigFile).String(),

		run: runCmd,

		list:             getCmd,
		listBackups:      getBackupsCmd,
		listNodes:        getNodesCmd,
		listNodesVerbose: getNodesCmd.Flag("verbose", "Include extra node info").Bool(),

		backup:               backupCmd,
		backupType:           backupCmd.Flag("backup-type", "Backup type").Enum("logical", "hot"),
		destinationType:      backupCmd.Flag("destination-type", "Backup destination type").Enum("file", "aws"),
		compressionAlgorithm: backupCmd.Flag("compression-algorithm", "Compression algorithm used for the backup").String(),
		encryptionAlgorithm:  backupCmd.Flag("encryption-algorithm", "Encryption algorithm used for the backup").String(),
		description:          backupCmd.Flag("description", "Backup description").Required().String(),

		restore: restoreCmd,
		restoreMetadataFile: restoreCmd.Arg("metadata-file", "Metadata file having the backup info for restore").
			HintAction(listAvailableBackups).Required().String(),
		restoreSkipUsersAndRoles: restoreCmd.Flag("skip-users-and-roles", "Do not restore users and roles").Default("true").Bool(),
	}

	app.Flag("tls", "Connection uses TLS if true, else plain TCP").Default("false").BoolVar(&opts.TLS)
	app.Flag("ca-file", "The file containning the CA root cert file").StringVar(&opts.CAFile)
	app.Flag("server-addr", "The server address in the format of host:port").Default(defaultServerAddr).StringVar(&opts.ServerAddr)

	yamlOpts := &cliOptions{
		ServerAddr: defaultServerAddr,
	}
	if *opts.configFile != "" {
		loadOptionsFromFile(defaultConfigFile, yamlOpts)
	}

	cmd, err := app.Parse(args)
	if err != nil {
		return "", nil, err
	}

	if cmd == "" {
		return "", opts, fmt.Errorf("Invalid command")
	}

	return cmd, opts, nil
}

func loadOptionsFromFile(filename string, opts *cliOptions) error {
	buf, err := ioutil.ReadFile(filename)
	if err != nil {
		return errors.Wrap(err, "cannot load configuration from file")
	}
	if err = yaml.Unmarshal(buf, opts); err != nil {
		return errors.Wrapf(err, "cannot unmarshal yaml file %s", filename)
	}
	return nil
}

func mergeOptions(opts, yamlOpts *cliOptions) {
	if opts.CAFile == "" {
		opts.CAFile = yamlOpts.CAFile
	}
	if opts.ServerAddr == "" {
		opts.ServerAddr = yamlOpts.ServerAddr
	}
}
