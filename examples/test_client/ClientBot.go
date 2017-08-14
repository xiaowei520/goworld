package main

import (
	"net"
	"sync"

	"fmt"

	"math/rand"

	"time"

	"reflect"

	"os"

	"github.com/xiaonanln/goworld/engine/common"
	"github.com/xiaonanln/goworld/engine/config"
	"github.com/xiaonanln/goworld/engine/consts"
	"github.com/xiaonanln/goworld/engine/entity"
	"github.com/xiaonanln/goworld/engine/gwlog"
	"github.com/xiaonanln/goworld/engine/netutil"
	"github.com/xiaonanln/goworld/engine/post"
	"github.com/xiaonanln/goworld/engine/proto"
	"golang.org/x/net/websocket"
)

const _SPACE_ENTITY_TYPE = "__space__"

// ClientBot is  a client bot representing a game client
type ClientBot struct {
	sync.Mutex

	id                 int
	waiter             *sync.WaitGroup
	conn               *proto.GoWorldConnection
	entities           map[common.EntityID]*clientEntity
	player             *clientEntity
	currentSpace       *ClientSpace
	logined            bool
	startedDoingThings bool
	syncPosTime        time.Time
	useWebSocket       bool
}

func newClientBot(id int, useWebSocket bool, waiter *sync.WaitGroup) *ClientBot {
	return &ClientBot{
		id:           id,
		waiter:       waiter,
		entities:     map[common.EntityID]*clientEntity{},
		useWebSocket: useWebSocket,
	}
}

func (bot *ClientBot) String() string {
	return fmt.Sprintf("ClientBot<%d>", bot.id)
}

func (bot *ClientBot) run() {
	defer bot.waiter.Done()

	gwlog.Infof("%s is running ...", bot)

	gateIDs := config.GetGateIDs()
	// choose a random gateid
	gateid := gateIDs[rand.Intn(len(gateIDs))]
	gwlog.Debugf("%s is connecting to gate %d", bot, gateid)
	cfg := config.GetGate(gateid)

	var netconn net.Conn
	var err error
	for { // retry for ever
		netconn, err = bot.connectServer(cfg)
		if err != nil {
			gwlog.Errorf("Connect failed: %s", err)
			time.Sleep(time.Second * time.Duration(1+rand.Intn(10)))
			continue
		}
		// connected , ok
		break
	}

	gwlog.Infof("connected: %s", netconn.RemoteAddr())

	var conn netutil.Connection = netutil.NetConnection{netconn}
	conn = netutil.NewBufferedReadConnection(conn)

	bot.conn = proto.NewGoWorldConnection(conn, cfg.CompressConnection)
	defer bot.conn.Close()

	bot.loop()
}

func (bot *ClientBot) connectServer(cfg *config.GateConfig) (net.Conn, error) {
	if bot.useWebSocket {
		origin := fmt.Sprintf("http://%s:%d/", serverAddr, cfg.HTTPPort)
		wsaddr := fmt.Sprintf("ws://%s:%d/ws", serverAddr, cfg.HTTPPort)

		wsConn, err := websocket.Dial(wsaddr, "", origin)
		return wsConn, err
	}

	conn, err := netutil.ConnectTCP(serverAddr, cfg.Port)
	if err != nil {
		conn.(*net.TCPConn).SetWriteBuffer(64 * 1024)
		conn.(*net.TCPConn).SetReadBuffer(64 * 1024)
	}
	return conn, err
}

func (bot *ClientBot) loop() {
	var msgtype proto.MsgType
	for {
		err := bot.conn.SetRecvDeadline(time.Now().Add(time.Millisecond * 100))
		if err != nil {
			gwlog.Panic(err)
		}

		pkt, err := bot.conn.Recv(&msgtype)

		if pkt != nil {
			bot.handlePacket(msgtype, pkt)
			pkt.Release()
		} else if err != nil && !netutil.IsTemporaryNetError(err) {
			// bad error
			gwlog.Panic(err)
		}

		if bot.player != nil && bot.player.TypeName == "Avatar" {
			now := time.Now()
			if now.Sub(bot.syncPosTime) > time.Millisecond*100 {
				player := bot.player
				const moveRange = 0.01
				if rand.Float32() < 0.5 { // let the posibility of avatar moving to be 50%
					player.pos.X += entity.Coord(-moveRange + moveRange*2*rand.Float32())
					player.pos.Z += entity.Coord(-moveRange + moveRange*rand.Float32())
					//gwlog.Infof("move to %f, %f", player.pos.X, player.pos.Z)
					player.yaw = entity.Yaw(rand.Float32() * 3.14)
					bot.conn.SendSyncPositionYawFromClient(player.ID, float32(player.pos.X), float32(player.pos.Y), float32(player.pos.Z), float32(player.yaw))
				}

				bot.syncPosTime = now
			}
		}

		bot.conn.Flush("ClientBot")
		post.Tick()
	}
}

func (bot *ClientBot) handlePacket(msgtype proto.MsgType, packet *netutil.Packet) {
	bot.Lock()
	defer bot.Unlock()

	if msgtype != proto.MT_CALL_FILTERED_CLIENTS && msgtype != proto.MT_SYNC_POSITION_YAW_ON_CLIENTS {
		_ = packet.ReadUint16()
		_ = packet.ReadClientID() // TODO: strip these two fields ? seems a little difficult, maybe later.
	}

	if msgtype == proto.MT_NOTIFY_MAP_ATTR_CHANGE_ON_CLIENT {
		entityID := packet.ReadEntityID()
		var path []interface{}
		packet.ReadData(&path)
		key := packet.ReadVarStr()
		var val interface{}
		packet.ReadData(&val)
		if !quiet {
			gwlog.Debugf("Entity %s Attribute %v: set %s=%v", entityID, path, key, val)
		}
		bot.applyMapAttrChange(entityID, path, key, val)
	} else if msgtype == proto.MT_NOTIFY_MAP_ATTR_DEL_ON_CLIENT {
		entityID := packet.ReadEntityID()
		var path []interface{}
		packet.ReadData(&path)
		key := packet.ReadVarStr()
		if !quiet {
			gwlog.Debugf("Entity %s Attribute %v deleted %s", entityID, path, key)
		}
		bot.applyMapAttrDel(entityID, path, key)
	} else if msgtype == proto.MT_NOTIFY_LIST_ATTR_CHANGE_ON_CLIENT {
		entityID := packet.ReadEntityID()
		var path []interface{}
		packet.ReadData(&path)
		index := packet.ReadUint32()
		var val interface{}
		packet.ReadData(&val)
		if !quiet {
			gwlog.Debugf("Entity %s Attribute %v: set [%d]=%v", entityID, path, index, val)
		}
		bot.applyListAttrChange(entityID, path, int(index), val)
	} else if msgtype == proto.MT_NOTIFY_LIST_ATTR_APPEND_ON_CLIENT {
		entityID := packet.ReadEntityID()
		var path []interface{}
		packet.ReadData(&path)
		var val interface{}
		packet.ReadData(&val)
		if !quiet {
			gwlog.Debugf("Entity %s Attribute %v: append %v", entityID, path, val)
		}
		bot.applyListAttrAppend(entityID, path, val)
	} else if msgtype == proto.MT_NOTIFY_LIST_ATTR_POP_ON_CLIENT {
		entityID := packet.ReadEntityID()
		var path []interface{}
		packet.ReadData(&path)
		if !quiet {
			gwlog.Debugf("Entity %s Attribute %v: pop", entityID, path)
		}
		bot.applyListAttrPop(entityID, path)
	} else if msgtype == proto.MT_CREATE_ENTITY_ON_CLIENT {
		isPlayer := packet.ReadBool()
		entityID := packet.ReadEntityID()
		typeName := packet.ReadVarStr()

		x := entity.Coord(packet.ReadFloat32())
		y := entity.Coord(packet.ReadFloat32())
		z := entity.Coord(packet.ReadFloat32())
		yaw := entity.Yaw(packet.ReadFloat32())
		//gwlog.Infof("Create e %s.%s: isPlayer=%v", typeName, entityID, isPlayer)
		var clientData map[string]interface{}
		packet.ReadData(&clientData)

		if typeName == _SPACE_ENTITY_TYPE {
			// this is a space
			bot.createSpace(entityID, clientData)
		} else {
			// this is a e
			bot.createEntity(typeName, entityID, isPlayer, clientData, x, y, z, yaw)
		}
	} else if msgtype == proto.MT_DESTROY_ENTITY_ON_CLIENT {
		typeName := packet.ReadVarStr()
		entityID := packet.ReadEntityID()
		if !quiet {
			gwlog.Debugf("Destroy e %s.%s", typeName, entityID)
		}
		if typeName == _SPACE_ENTITY_TYPE {
			bot.destroySpace(entityID)
		} else {
			bot.destroyEntity(typeName, entityID)
		}
	} else if msgtype == proto.MT_CALL_ENTITY_METHOD_ON_CLIENT {
		entityID := packet.ReadEntityID()
		method := packet.ReadVarStr()
		args := packet.ReadArgs()
		if !quiet {
			gwlog.Debugf("Call e %s.%s(%v)", entityID, method, args)
		}
		bot.callEntityMethod(entityID, method, args)
	} else if msgtype == proto.MT_CALL_FILTERED_CLIENTS {
		_ = packet.ReadVarStr() // ignore key
		_ = packet.ReadVarStr() // ignore val
		method := packet.ReadVarStr()
		args := packet.ReadArgs()
		if bot.player == nil {
			gwlog.Warnf("Player not found while calling filtered client")
			return
		}

		bot.callEntityMethod(bot.player.ID, method, args)
	} else if msgtype == proto.MT_UPDATE_POSITION_ON_CLIENT {
		entityID := packet.ReadEntityID()
		x := entity.Coord(packet.ReadFloat32())
		y := entity.Coord(packet.ReadFloat32())
		z := entity.Coord(packet.ReadFloat32())
		bot.updateEntityPosition(entityID, entity.Position{x, y, z})
	} else if msgtype == proto.MT_UPDATE_YAW_ON_CLIENT { // todo: this msgtype useful ?
		entityID := packet.ReadEntityID()
		yaw := entity.Yaw(packet.ReadFloat32())
		bot.updateEntityYaw(entityID, yaw)
	} else if msgtype == proto.MT_SYNC_POSITION_YAW_ON_CLIENTS {
		for packet.HasUnreadPayload() {
			entityID := packet.ReadEntityID()
			x := entity.Coord(packet.ReadFloat32())
			y := entity.Coord(packet.ReadFloat32())
			z := entity.Coord(packet.ReadFloat32())
			yaw := entity.Yaw(packet.ReadFloat32())
			bot.updateEntityPosition(entityID, entity.Position{x, y, z})
			bot.updateEntityYaw(entityID, yaw)
		}
	} else {
		gwlog.Panicf("unknown msgtype: %v", msgtype)
		if consts.DEBUG_MODE {
			os.Exit(2)
		}
	}
}
func (bot *ClientBot) updateEntityPosition(entityID common.EntityID, position entity.Position) {
	//gwlog.Debugf("updateEntityPosition %s => %s", entityID, position)
	if bot.entities[entityID] == nil {
		gwlog.Errorf("e %s not found", entityID)
		return
	}
	entity := bot.entities[entityID]
	entity.pos = position
}

func (bot *ClientBot) updateEntityYaw(entityID common.EntityID, yaw entity.Yaw) {
	//gwlog.Debugf("updateEntityYaw %s => %s", entityID, yaw)
	if bot.entities[entityID] == nil {
		gwlog.Errorf("e %s not found", entityID)
		return
	}
	entity := bot.entities[entityID]
	entity.yaw = yaw
}

func (bot *ClientBot) applyMapAttrChange(entityID common.EntityID, path []interface{}, key string, val interface{}) {
	//gwlog.Infof("SET ATTR %s.%v: set %s=%v", entityID, path, key, val)
	if bot.entities[entityID] == nil {
		gwlog.Errorf("entity %s not found", entityID)
		return
	}
	entity := bot.entities[entityID]
	entity.applyMapAttrChange(path, key, val)
}

func (bot *ClientBot) applyMapAttrDel(entityID common.EntityID, path []interface{}, key string) {
	//gwlog.Infof("DEL ATTR %s.%v: del %s", entityID, path, key)
	if bot.entities[entityID] == nil {
		gwlog.Errorf("entity %s not found", entityID)
		return
	}
	entity := bot.entities[entityID]
	entity.applyMapAttrDel(path, key)
}

func (bot *ClientBot) applyListAttrChange(entityID common.EntityID, path []interface{}, index int, val interface{}) {
	if bot.entities[entityID] == nil {
		gwlog.Errorf("entity %s not found", entityID)
		return
	}
	entity := bot.entities[entityID]
	entity.applyListAttrChange(path, index, val)
}

func (bot *ClientBot) applyListAttrAppend(entityID common.EntityID, path []interface{}, val interface{}) {
	if bot.entities[entityID] == nil {
		gwlog.Errorf("entity %s not found", entityID)
		return
	}
	entity := bot.entities[entityID]
	entity.applyListAttrAppend(path, val)
}

func (bot *ClientBot) applyListAttrPop(entityID common.EntityID, path []interface{}) {
	if bot.entities[entityID] == nil {
		gwlog.Errorf("entity %s not found", entityID)
		return
	}
	entity := bot.entities[entityID]
	entity.applyListAttrPop(path)

}

func (bot *ClientBot) createEntity(typeName string, entityID common.EntityID, isPlayer bool, clientData map[string]interface{}, x, y, z entity.Coord, yaw entity.Yaw) {
	if bot.entities[entityID] == nil {
		e := newClientEntity(bot, typeName, entityID, isPlayer, clientData, x, y, z, yaw)
		bot.entities[entityID] = e
		if isPlayer {
			if bot.player != nil {
				gwlog.TraceError("%s.createEntity: creating player %S, but player is already set to %s", bot, e, bot.player)
			}
			bot.player = e
		}
	}
}

func (bot *ClientBot) destroyEntity(typeName string, entityID common.EntityID) {
	entity := bot.entities[entityID]
	if entity != nil {
		entity.Destroy()
		if entity == bot.player {
			bot.player = nil
		}
		delete(bot.entities, entityID)
	}
}

func (bot *ClientBot) createSpace(spaceID common.EntityID, data map[string]interface{}) {
	if bot.currentSpace != nil {
		gwlog.TraceError("%s.createSpace: duplicate space: %s and %s", bot, bot.currentSpace, spaceID)
	}
	space := newClientSpace(bot, spaceID, data)
	bot.currentSpace = space
	gwlog.Debugf("%s current space change to %s", bot, space)
	bot.OnEnterSpace()
}

func (bot *ClientBot) destroySpace(spaceID common.EntityID) {
	if bot.currentSpace == nil || bot.currentSpace.ID != spaceID {
		gwlog.TraceError("%s.destroySpace: space %s not exists, current space is %s", bot, spaceID, bot.currentSpace)
		return
	}
	oldSpace := bot.currentSpace
	bot.currentSpace = nil
	gwlog.Debugf("%s: leave current space %s", bot, spaceID)
	bot.OnLeaveSpace(oldSpace)
}

func (bot *ClientBot) callEntityMethod(entityID common.EntityID, method string, args [][]byte) {
	entity := bot.entities[entityID]
	if entity == nil {
		gwlog.Warnf("Entity %s is not found while calling method %s(%v)", entityID, method, args)
		return
	}

	methodVal := reflect.ValueOf(entity).MethodByName(method)
	if !methodVal.IsValid() {
		gwlog.Errorf("Client method %s is not found", method)
		return
	}

	methodType := methodVal.Type()
	in := make([]reflect.Value, len(args))

	for i, arg := range args {
		argType := methodType.In(i)
		argValPtr := reflect.New(argType)
		netutil.MSG_PACKER.UnpackMsg(arg, argValPtr.Interface())
		in[i] = reflect.Indirect(argValPtr)
	}
	methodVal.Call(in)
}

func (bot *ClientBot) username() string {
	return fmt.Sprintf("test%d", bot.id)
}

func (bot *ClientBot) password() string {
	return "123456"
}

// CallServer calls server method of target e
func (bot *ClientBot) CallServer(id common.EntityID, method string, args []interface{}) {
	if !quiet {
		gwlog.Debugf("%s call server: %s.%s%v", bot, id, method, args)
	}
	bot.conn.SendCallEntityMethodFromClient(id, method, args)
}

// OnEnterSpace is called when player enters space
func (bot *ClientBot) OnEnterSpace() {
	gwlog.Debugf("%s.OnEnterSpace, player=%s", bot, bot.player)
	player := bot.player
	if !bot.startedDoingThings {
		bot.startedDoingThings = true
		player.doSomethingLater()
	} else {
		player.notifyThingDone("DoEnterRandomSpace")
	}
}

// OnLeaveSpace is called when player leaves space
func (bot *ClientBot) OnLeaveSpace(oldSpace *ClientSpace) {
	gwlog.Debugf("%s.OnLeaveSpace, player=%s", bot, bot.player)
}
