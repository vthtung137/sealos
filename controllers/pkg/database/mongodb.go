// Copyright © 2023 sealos.
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

package database

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	gonanoid "github.com/matoous/go-nanoid/v2"

	accountv1 "github.com/labring/sealos/controllers/account/api/v1"
	"github.com/labring/sealos/controllers/pkg/crypto"
	"github.com/labring/sealos/controllers/pkg/resources"
	"github.com/labring/sealos/controllers/pkg/utils/logger"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"golang.org/x/sync/errgroup"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	DefaultDBName         = "sealos-resources"
	DefaultMeteringConn   = "metering"
	DefaultMonitorConn    = "monitor"
	DefaultBillingConn    = "billing"
	DefaultPricesConn     = "prices"
	DefaultPropertiesConn = "properties"
)

const DefaultRetentionDay = 30

const (
	MongoURI           = "MONGO_URI"
	MongoUsername      = "MONGO_USERNAME"
	MongoPassword      = "MONGO_PASSWORD"
	RetentionDay       = "RETENTION_DAY"
	PermanentRetention = "PERMANENT_RETENTION"
)

// override this value at build time
const defaultCryptoKey = "Af0b2Bc5e9d0C84adF0A5887cF43aB63"

var cryptoKey = defaultCryptoKey

type MongoDB struct {
	URL               string
	Client            *mongo.Client
	DBName            string
	MonitorConnPrefix string
	MeteringConn      string
	BillingConn       string
	PricesConn        string
	PropertiesConn    string
}

type AccountBalanceSpecBSON struct {
	// Time    metav1.Time `json:"time" bson:"time"`
	// If the Time field is of the time. time type, it cannot be converted to json crd, so use metav1.Time. However, metav1.Time cannot be inserted into mongo, so you need to convert it to time.Time
	Time                                   time.Time `json:"time" bson:"time"`
	accountv1.BillingRecordQueryItemInline `json:",inline" bson:",inline"`
}

func (m *MongoDB) Disconnect(ctx context.Context) error {
	return m.Client.Disconnect(ctx)
}

func (m *MongoDB) GetBillingLastUpdateTime(owner string, _type accountv1.Type) (bool, time.Time, error) {
	filter := bson.M{
		"owner": owner,
		"type":  _type,
	}
	findOneOptions := options.FindOne().SetSort(bson.D{primitive.E{Key: "time", Value: -1}})
	var result bson.M
	err := m.getBillingCollection().FindOne(context.Background(), filter, findOneOptions).Decode(&result)

	if err != nil {
		if err == mongo.ErrNoDocuments {
			return false, time.Time{}, nil
		}
		return false, time.Time{}, err
	}
	// Assuming that the `time` field is stored as a `primitive.DateTime`
	if resultTime, ok := result["time"].(primitive.DateTime); ok {
		return true, resultTime.Time().UTC(), nil
	}

	return false, time.Time{}, fmt.Errorf("failed to convert time field to primitive.DateTime: %v", result["time"])
}

func (m *MongoDB) GetUnsettingBillingHandler(owner string) ([]resources.BillingHandler, error) {
	filter := bson.M{
		"owner": owner,
		"status": bson.M{
			"$in": []resources.BillingStatus{
				resources.Unsettled,
			},
		},
	}
	findOptions := options.Find()
	cur, err := m.getBillingCollection().Find(context.Background(), filter, findOptions)
	if err != nil {
		return nil, fmt.Errorf("find error: %v", err)
	}
	defer cur.Close(context.Background())
	var results []resources.BillingHandler
	for cur.Next(context.Background()) {
		var result resources.BillingHandler
		if err := cur.Decode(&result); err != nil {
			return nil, fmt.Errorf("decode error: %v", err)
		}
		results = append(results, result)
	}
	if err := cur.Err(); err != nil {
		return nil, fmt.Errorf("cursor error: %v", err)
	}
	return results, nil
}

func (m *MongoDB) UpdateBillingStatus(orderID string, status resources.BillingStatus) error {
	// create a query filter
	filter := bson.M{"order_id": orderID}
	update := bson.M{
		"$set": bson.M{
			"status": status,
		},
	}
	_, err := m.getBillingCollection().UpdateOne(context.Background(), filter, update)
	if err != nil {
		return fmt.Errorf("update error: %v", err)
	}
	return nil
}

func (m *MongoDB) GetBillingHistoryNamespaces(startTime, endTime *time.Time, billType int, owner string) ([]string, error) {
	filter := bson.M{
		"owner": owner,
	}
	if startTime != nil && endTime != nil {
		filter["time"] = bson.M{
			"$gte": startTime.UTC(),
			"$lte": endTime.UTC(),
		}
	}
	if billType != -1 {
		filter["type"] = billType
	}

	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: filter}},
		{{Key: "$group", Value: bson.D{{Key: "_id", Value: nil}, {Key: "namespaces", Value: bson.D{{Key: "$addToSet", Value: "$namespace"}}}}}},
	}

	cur, err := m.getBillingCollection().Aggregate(context.Background(), pipeline)
	if err != nil {
		return nil, err
	}
	defer cur.Close(context.Background())

	if !cur.Next(context.Background()) {
		return []string{}, nil
	}

	var result struct {
		Namespaces []string `bson:"namespaces"`
	}
	if err := cur.Decode(&result); err != nil {
		return nil, err
	}
	return result.Namespaces, nil
}

func (m *MongoDB) GetBillingHistoryNamespaceList(nsHistorySpec *accountv1.NamespaceBillingHistorySpec, owner string) ([]string, error) {
	filter := bson.M{
		"owner": owner,
	}
	if nsHistorySpec.StartTime != nsHistorySpec.EndTime {
		filter["time"] = bson.M{
			"$gte": nsHistorySpec.StartTime.Time.UTC(),
			"$lte": nsHistorySpec.EndTime.Time.UTC(),
		}
	}
	if nsHistorySpec.Type != -1 {
		filter["type"] = nsHistorySpec.Type
	}

	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: filter}},
		{{Key: "$group", Value: bson.D{{Key: "_id", Value: nil}, {Key: "namespaces", Value: bson.D{{Key: "$addToSet", Value: "$namespace"}}}}}},
	}

	cur, err := m.getBillingCollection().Aggregate(context.Background(), pipeline)
	if err != nil {
		return nil, err
	}
	defer cur.Close(context.Background())

	if !cur.Next(context.Background()) {
		return []string{}, nil
	}

	var result struct {
		Namespaces []string `bson:"namespaces"`
	}
	if err := cur.Decode(&result); err != nil {
		return nil, err
	}
	return result.Namespaces, nil
}

func (m *MongoDB) SaveBillings(billing ...*resources.Billing) error {
	billings := make([]interface{}, len(billing))
	for i, b := range billing {
		billings[i] = b
	}
	_, err := m.getBillingCollection().InsertMany(context.Background(), billings)
	return err
}

func (m *MongoDB) GetMeteringOwnerTimeResult(queryTime time.Time, queryCategories, queryProperties []string) (*MeteringOwnerTimeResult, error) {
	matchValue := bson.M{
		"time":     queryTime,
		"category": bson.M{"$in": queryCategories},
	}
	if len(queryProperties) > 0 {
		matchValue["property"] = bson.M{"$in": queryProperties}
	}
	pipeline := bson.A{
		bson.D{{Key: "$match", Value: matchValue}},
		bson.D{{Key: "$group", Value: bson.M{
			"_id":           bson.M{"property": "$property"},
			"propertyTotal": bson.M{"$sum": "$amount"},
		}}},
		bson.D{{Key: "$project", Value: bson.M{
			"_id":           0,
			"property":      "$_id.property",
			"propertyTotal": 1,
		}}},
		bson.D{{Key: "$group", Value: bson.M{
			"_id":         nil,
			"amountTotal": bson.M{"$sum": "$propertyTotal"},
			"costs":       bson.M{"$push": bson.M{"k": "$property", "v": "$propertyTotal"}},
		}}},
		bson.D{{Key: "$addFields", Value: bson.M{
			//"owner":  queryOwner,
			"time":   queryTime,
			"amount": "$amountTotal",
			"costs":  bson.M{"$arrayToObject": "$costs"},
		}}},
	}

	/*
		db.metering.aggregate([
		{ $match:
		  { time: queryTime, category:
		     { $in: ["ns-gxqoxr8s"] }, property: { $in: ["cpu", "memory", "storage"] } } },
		{ $group: { _id: { property: "$property" }, propertyTotal: { $sum: "$amount" } } },
		{ $project: { _id: 0, property: "$_id.property", propertyTotal: 1 } },
		{ $group: { _id: null, amountTotal: { $sum: "$propertyTotal" }, costs: { $push: { k: "$property", v: "$propertyTotal" } } } },
		{ $addFields: { orderId: "111111111", own: queryOwn, time: queryTime, type: 0, amount: "$amountTotal", costs: { $arrayToObject: "$costs" } } },
		{ $out: "results1" }]);
	*/
	cursor, err := m.getMeteringCollection().Aggregate(context.Background(), pipeline)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(context.Background())
	if cursor.Next(context.Background()) {
		var result MeteringOwnerTimeResult
		if err = cursor.Decode(&result); err != nil {
			return nil, err
		}
		return &result, nil
	}
	return nil, nil
}

// InsertMonitor insert monitor data to mongodb collection monitor + time (eg: monitor_20200101)
// The monitor data is saved daily 2020-12-01 00:00:00 - 2020-12-01 23:59:59 => monitor_20201201
func (m *MongoDB) InsertMonitor(ctx context.Context, monitors ...*resources.Monitor) error {
	if len(monitors) == 0 {
		return nil
	}
	var manyMonitor []interface{}
	for i := range monitors {
		manyMonitor = append(manyMonitor, monitors[i])
	}
	_, err := m.getMonitorCollection(monitors[0].Time).InsertMany(ctx, manyMonitor)
	return err
}

func (m *MongoDB) GetAllPricesMap() (map[string]resources.Price, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cursor, err := m.getPricesCollection().Find(ctx, bson.M{})
	if err != nil {
		return nil, fmt.Errorf("get all prices error: %v", err)
	}
	var prices []struct {
		Property string `json:"property" bson:"property"`
		Price    string `json:"price" bson:"price"`
		Detail   string `json:"detail" bson:"detail"`
	}
	if err = cursor.All(ctx, &prices); err != nil {
		return nil, fmt.Errorf("get all prices error: %v", err)
	}
	var pricesMap = make(map[string]resources.Price, len(prices))
	for i := range prices {
		price, err := crypto.DecryptInt64WithKey(prices[i].Price, []byte(cryptoKey))
		if err != nil {
			return nil, fmt.Errorf("decrypt price error: %v", err)
		}
		pricesMap[prices[i].Property] = resources.Price{
			Price:    price,
			Detail:   prices[i].Detail,
			Property: prices[i].Property,
		}
	}
	return pricesMap, nil
}

func (m *MongoDB) InitDefaultPropertyTypeLS() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cursor, err := m.getPropertiesCollection().Find(ctx, bson.M{})
	if err != nil {
		return fmt.Errorf("get all prices error: %v", err)
	}
	var properties []resources.PropertyType
	if err = cursor.All(ctx, &properties); err != nil {
		return fmt.Errorf("get all prices error: %v", err)
	}
	if len(properties) != 0 {
		resources.DefaultPropertyTypeLS = resources.NewPropertyTypeLS(properties)
	}
	return nil
}

func (m *MongoDB) SavePropertyTypes(types []resources.PropertyType) error {
	tps := make([]interface{}, len(types))
	for i, b := range types {
		tps[i] = b
	}
	_, err := m.getPropertiesCollection().InsertMany(context.Background(), tps)
	return err
}

// 2020-12-01 23:00:00 - 2020-12-02 00:00:00
// 2020-12-02 00:00:00 - 2020-12-02 01:00:00
func (m *MongoDB) GenerateMeteringData(startTime, endTime time.Time, prices map[string]resources.Price) error {
	filter := bson.M{
		"time": bson.M{
			"$gte": startTime,
			"$lt":  endTime,
		},
	}
	cursor, err := m.getMonitorCollection(startTime).Find(context.Background(), filter)
	if err != nil {
		return fmt.Errorf("find monitors error: %v", err)
	}
	defer cursor.Close(context.Background())

	meteringMap := make(map[string]map[string]int64)
	countMap := make(map[string]map[string]int64)
	updateTimeMap := make(map[string]map[string]*time.Time)

	for cursor.Next(context.Background()) {
		var monitor resources.Monitor
		if err := cursor.Decode(&monitor); err != nil {
			return fmt.Errorf("decode monitor error: %v", err)
		}

		if _, ok := updateTimeMap[monitor.Category]; !ok {
			updateTimeMap[monitor.Category] = make(map[string]*time.Time)
		}
		if _, ok := updateTimeMap[monitor.Category][monitor.Property]; !ok {
			lastUpdateTime, err := m.GetUpdateTimeForCategoryAndPropertyFromMetering(monitor.Category, monitor.Property)
			if err != nil {
				logger.Debug(err, "get latest update time failed", "category", monitor.Category, "property", monitor.Property)
			}
			updateTimeMap[monitor.Category][monitor.Property] = &lastUpdateTime
		}
		lastUpdateTime := updateTimeMap[monitor.Category][monitor.Property].UTC()

		if /* skip last update lte 1 hour*/ lastUpdateTime.Before(startTime) || lastUpdateTime.Equal(startTime) {
			if _, ok := meteringMap[monitor.Category]; !ok {
				meteringMap[monitor.Category] = make(map[string]int64)
				countMap[monitor.Category] = make(map[string]int64)
			}
			//TODO interface will delete
			//meteringMap[monitor.Category][monitor.Property] += monitor.Value
			countMap[monitor.Category][monitor.Property]++
			continue
		}
		logger.Debug("Info", "skip metering", "category", monitor.Category, "property", monitor.Property, "lastUpdateTime", updateTimeMap[monitor.Category][monitor.Property].UTC(), "startTime", startTime)
	}

	if err := cursor.Err(); err != nil {
		return fmt.Errorf("cursor error: %v", err)
	}
	eg, _ := errgroup.WithContext(context.Background())

	for category, propertyMap := range meteringMap {
		for property, totalValue := range propertyMap {
			count := countMap[category][property]
			if count < 60 {
				count = 60
			}
			unitValue := math.Ceil(float64(totalValue) / float64(count))
			metering := &resources.Metering{
				Category: category,
				Property: property,
				Time:     endTime,
				Amount:   int64(unitValue * float64(prices[property].Price)),
				Value:    int64(unitValue),
				//Detail:   "",
			}
			_category, _property := category, property
			eg.Go(func() error {
				_, err := m.getMeteringCollection().InsertOne(context.Background(), metering)
				if err != nil {
					//TODO if insert failed, should todo?
					logger.Error(err, "insert metering data failed", "category", _category, "property", _property)
				}
				return err
			})
		}
	}
	return eg.Wait()
}

/*
		monitors = append(monitors, &common.Monitor{
		Category: namespace.Name,
		Used:     getResourceUsed(podResource),
		Time:     timeStamp,
		Type:     resourceMap[name].Type(),
		Name:     resourceMap[name].Name(),
	})
*/
func (m *MongoDB) GenerateBillingData(startTime, endTime time.Time, prols *resources.PropertyTypeLS, namespaces []string, owner string) (orderID []string, amount int64, err error) {
	minutes := endTime.Sub(startTime).Minutes()

	groupStage := bson.D{
		primitive.E{Key: "_id", Value: bson.D{{Key: "type", Value: "$type"}, {Key: "name", Value: "$name"}, {Key: "category", Value: "$category"}}},
		primitive.E{Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}},
	}

	projectStage := bson.D{
		primitive.E{Key: "_id", Value: 0},
		primitive.E{Key: "type", Value: "$_id.type"},
		primitive.E{Key: "name", Value: "$_id.name"},
		primitive.E{Key: "category", Value: "$_id.category"},
	}

	// initialize the used phase
	usedStage := bson.M{}

	// Build the $group and $project phases dynamically from EnumMap
	for key, value := range prols.EnumMap {
		keyStr := strconv.Itoa(int(key))

		// $max - $min;
		// When max is not zero, the minimum value other than the zero value is used to prevent some data from obtaining a value in special cases
		// max-min=0 if the hour has only one data piece or no data piece
		if value.PriceType == resources.DIF {
			// for non 0 $min
			minWithCondition := bson.D{
				{Key: "$min", Value: bson.D{
					{Key: "$cond", Value: bson.A{
						bson.D{{Key: "$eq", Value: bson.A{"$used." + keyStr, 0}}},
						nil, // 将0值排除在外
						"$used." + keyStr,
					}},
				}},
			}

			groupStage = append(groupStage,
				primitive.E{Key: keyStr + "_max", Value: bson.D{{Key: "$max", Value: "$used." + keyStr}}}, // 正常计算$max
				primitive.E{Key: keyStr + "_min", Value: minWithCondition},
			)

			// added to the used phase
			usedStage[keyStr] = bson.D{{Key: "$subtract", Value: bson.A{
				"$" + keyStr + "_max",
				"$" + keyStr + "_min",
			}}}
			continue
		}
		groupStage = append(groupStage, primitive.E{Key: keyStr, Value: bson.D{{Key: "$sum", Value: "$used." + keyStr}}})
		usedStage[keyStr] = bson.D{{Key: "$toInt", Value: bson.D{{Key: "$round", Value: bson.D{{Key: "$divide", Value: bson.A{
			"$" + keyStr, bson.D{{Key: "$cond", Value: bson.A{bson.D{{Key: "$gt", Value: bson.A{"$count", minutes}}}, "$count", minutes}}}}}}}}}}
	}

	// add the used phase to the $project phase
	projectStage = append(projectStage, primitive.E{Key: "used", Value: usedStage})

	// construction-pipeline
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{{Key: "time", Value: bson.D{{Key: "$gte", Value: startTime}, {Key: "$lt", Value: endTime}}}, {Key: "category", Value: bson.D{{Key: "$in", Value: namespaces}}}}}},
		{{Key: "$group", Value: groupStage}},
		{{Key: "$project", Value: projectStage}},
	}

	cursor, err := m.getMonitorCollection(startTime).Aggregate(context.Background(), pipeline)
	if err != nil {
		return nil, 0, fmt.Errorf("aggregate error: %v", err)
	}
	defer cursor.Close(context.Background())

	var appCostsMap = make(map[string]map[uint8][]resources.AppCost)
	// map[ns/type]int64
	var nsTypeAmount = make(map[string]int64)

	for cursor.Next(context.Background()) {
		var result struct {
			Type      uint8                 `bson:"type"`
			Namespace string                `bson:"category"`
			Name      string                `bson:"name"`
			Used      resources.EnumUsedMap `bson:"used"`
		}

		err := cursor.Decode(&result)
		if err != nil {
			return nil, 0, fmt.Errorf("decode error: %v", err)
		}

		//TODO delete
		//logger.Info("generate billing data", "result", result)

		if _, ok := appCostsMap[result.Namespace]; !ok {
			appCostsMap[result.Namespace] = make(map[uint8][]resources.AppCost)
		}
		if _, ok := appCostsMap[result.Namespace][result.Type]; !ok {
			appCostsMap[result.Namespace][result.Type] = make([]resources.AppCost, 0)
		}
		appCost := resources.AppCost{
			Used:       result.Used,
			Name:       result.Name,
			UsedAmount: make(map[uint8]int64),
		}
		// Calculate the amount and set the used value
		for property := range result.Used {
			if prop, ok := prols.EnumMap[property]; ok {
				if prop.UnitPrice > 0 {
					appCost.UsedAmount[property] = int64(math.Ceil(float64(result.Used[property]) / prop.UnitPrice))
					appCost.Amount += appCost.UsedAmount[property]
				}
			}
		}
		if appCost.Amount == 0 {
			continue
		}
		nsTypeAmount[result.Namespace+strconv.Itoa(int(result.Type))] += appCost.Amount
		appCostsMap[result.Namespace][result.Type] = append(appCostsMap[result.Namespace][result.Type], appCost)
	}

	for ns, appCostMap := range appCostsMap {
		for tp, appCost := range appCostMap {
			amountt := nsTypeAmount[ns+strconv.Itoa(int(tp))]
			if amountt == 0 {
				continue
			}
			id, err := gonanoid.New(12)
			if err != nil {
				return nil, 0, fmt.Errorf("generate billing id error: %v", err)
			}
			billing := resources.Billing{
				OrderID:   id,
				Type:      accountv1.Consumption,
				Namespace: ns,
				AppType:   tp,
				AppCosts:  appCost,
				Amount:    amountt,
				Owner:     owner,
				Time:      endTime,
				Status:    resources.Settled,
			}
			amount += amountt
			orderID = append(orderID, id)
			// Insert the billing document
			_, err = m.getBillingCollection().InsertOne(context.Background(), billing)
			if err != nil {
				return nil, 0, fmt.Errorf("insert error: %v", err)
			}
			//TODO delete
			//logger.Info("generate billing data", "billing", billing)
		}
	}

	if err = cursor.Err(); err != nil {
		return nil, 0, fmt.Errorf("cursor error: %v", err)
	}
	return orderID, amount, nil
}

func (m *MongoDB) GetUpdateTimeForCategoryAndPropertyFromMetering(category string, property string) (time.Time, error) {
	filter := bson.M{"category": category, "property": property}
	// sort by time desc
	opts := options.FindOne().SetSort(bson.D{primitive.E{Key: "time", Value: -1}})

	var result struct {
		Time time.Time `bson:"time"`
	}
	err := m.getMeteringCollection().FindOne(context.Background(), filter, opts).Decode(&result)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			// No documents match the filter. Handle this case accordingly.
			return time.Time{}, nil
		}
		return time.Time{}, err
	}
	return result.Time, nil
}

func (m *MongoDB) queryBillingRecordsByOrderID(billingRecordQuery *accountv1.BillingRecordQuery, owner string) error {
	if billingRecordQuery.Spec.OrderID == "" {
		return fmt.Errorf("order id is empty")
	}
	billingColl := m.getBillingCollection()
	matchStage := bson.D{
		primitive.E{Key: "$match", Value: bson.D{
			primitive.E{Key: "order_id", Value: billingRecordQuery.Spec.OrderID},
			primitive.E{Key: "owner", Value: owner},
		}},
	}
	var billingRecords []accountv1.BillingRecordQueryItem
	ctx := context.Background()

	cursor, err := billingColl.Aggregate(ctx, bson.A{matchStage})
	if err != nil {
		return fmt.Errorf("failed to execute aggregate query: %w", err)
	}
	defer cursor.Close(ctx)

	for cursor.Next(ctx) {
		var bsonRecord resources.Billing
		if err := cursor.Decode(&bsonRecord); err != nil {
			return fmt.Errorf("failed to decode billing record: %w", err)
		}
		var billingRecord = accountv1.BillingRecordQueryItem{
			Time: metav1.NewTime(bsonRecord.Time),
			BillingRecordQueryItemInline: accountv1.BillingRecordQueryItemInline{
				OrderID:   bsonRecord.OrderID,
				Type:      bsonRecord.Type,
				Amount:    bsonRecord.Amount,
				Namespace: bsonRecord.Namespace,
			},
		}
		switch bsonRecord.Type {
		case accountv1.Recharge:
			paymentAmount := billingRecord.Amount
			if bsonRecord.Payment != nil {
				paymentAmount = bsonRecord.Payment.Amount
			}
			billingRecord.Payment = &accountv1.PaymentForQuery{Amount: paymentAmount}
			billingRecords = append(billingRecords, billingRecord)
		case accountv1.TransferOut, accountv1.TransferIn:
			billingRecords = append(billingRecords, billingRecord)
		default:
			for _, cost := range bsonRecord.AppCosts {
				billingRecord = accountv1.BillingRecordQueryItem{
					Time: metav1.NewTime(bsonRecord.Time),
					BillingRecordQueryItemInline: accountv1.BillingRecordQueryItemInline{
						OrderID:   bsonRecord.OrderID,
						Type:      bsonRecord.Type,
						Namespace: bsonRecord.Namespace,
						AppType:   resources.AppTypeReverse[bsonRecord.AppType],
						Costs:     resources.ConvertEnumUsedToString(cost.UsedAmount),
						Amount:    cost.Amount,
						Name:      cost.Name,
					},
				}
				billingRecords = append(billingRecords, billingRecord)
			}
		}
	}

	billingRecordQuery.Status.Items = billingRecords
	billingRecordQuery.Status.PageLength = 1
	billingRecordQuery.Status.TotalCount = len(billingRecords)
	return nil
}

func (m *MongoDB) QueryBillingRecords(billingRecordQuery *accountv1.BillingRecordQuery, owner string) (err error) {
	if billingRecordQuery.Spec.OrderID != "" {
		return m.queryBillingRecordsByOrderID(billingRecordQuery, owner)
	}
	if owner == "" {
		return fmt.Errorf("owner is empty")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	billingColl := m.getBillingCollection()
	timeMatchValue := bson.D{primitive.E{Key: "$gte", Value: billingRecordQuery.Spec.StartTime.Time}, primitive.E{Key: "$lte", Value: billingRecordQuery.Spec.EndTime.Time}}
	matchValue := bson.D{
		primitive.E{Key: "time", Value: timeMatchValue},
		primitive.E{Key: "owner", Value: owner},
	}
	if billingRecordQuery.Spec.Type != -1 {
		matchValue = append(matchValue, primitive.E{Key: "type", Value: billingRecordQuery.Spec.Type})
	}
	if billingRecordQuery.Spec.Namespace != "" {
		matchValue = append(matchValue, primitive.E{Key: "namespace", Value: billingRecordQuery.Spec.Namespace})
	}
	if billingRecordQuery.Spec.AppType != "" {
		matchValue = append(matchValue, primitive.E{Key: "app_type", Value: resources.AppType[strings.ToUpper(billingRecordQuery.Spec.AppType)]})
	}
	matchStage := bson.D{
		primitive.E{
			Key: "$match", Value: matchValue,
		},
	}

	// Pipeline for getting the paginated data
	pipeline := bson.A{
		matchStage,
		bson.D{primitive.E{Key: "$sort", Value: bson.D{primitive.E{Key: "time", Value: -1}}}},
		bson.D{primitive.E{Key: "$skip", Value: (billingRecordQuery.Spec.Page - 1) * billingRecordQuery.Spec.PageSize}},
		bson.D{primitive.E{Key: "$limit", Value: billingRecordQuery.Spec.PageSize}},
	}

	pipelineAll := bson.A{
		matchStage,
		bson.D{primitive.E{Key: "$group", Value: bson.D{
			primitive.E{Key: "_id", Value: nil},
			primitive.E{Key: "result", Value: bson.D{primitive.E{Key: "$sum", Value: 1}}},
		}}},
	}

	pipelineCountAndAmount := bson.A{
		bson.D{{Key: "$match", Value: bson.D{
			{Key: "time", Value: timeMatchValue},
			{Key: "owner", Value: owner},
			{Key: "type", Value: accountv1.Consumption},
		}}},
		bson.D{{Key: "$addFields", Value: bson.D{
			{Key: "costsArray", Value: bson.D{{Key: "$objectToArray", Value: "$costs"}}},
		}}},
		bson.D{{Key: "$unwind", Value: "$costsArray"}},
		bson.D{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: bson.D{
				{Key: "type", Value: "$type"},
				{Key: "key", Value: "$costsArray.k"},
			}},
			{Key: "total", Value: bson.D{{Key: "$sum", Value: "$costsArray.v"}}},
			{Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}},
		}}},
	}

	pipelineRechargeAmount := bson.A{
		bson.D{{Key: "$match", Value: bson.D{
			{Key: "time", Value: timeMatchValue},
			{Key: "owner", Value: owner},
			{Key: "type", Value: accountv1.Recharge},
		}}},
		bson.D{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: nil},
			{Key: "totalRechargeAmount", Value: bson.D{{Key: "$sum", Value: "$amount"}}},
			{Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}},
		}}},
	}

	cursor, err := billingColl.Aggregate(ctx, pipeline)
	if err != nil {
		return fmt.Errorf("failed to execute aggregate query: %w", err)
	}
	defer cursor.Close(ctx)

	var billingRecords []accountv1.BillingRecordQueryItem
	for cursor.Next(ctx) {
		var bsonRecord resources.Billing
		if err := cursor.Decode(&bsonRecord); err != nil {
			return fmt.Errorf("failed to decode billing record: %w", err)
		}
		billingRecord := accountv1.BillingRecordQueryItem{
			Time: metav1.NewTime(bsonRecord.Time),
			BillingRecordQueryItemInline: accountv1.BillingRecordQueryItemInline{
				OrderID:   bsonRecord.OrderID,
				Namespace: bsonRecord.Namespace,
				Type:      bsonRecord.Type,
				AppType:   resources.AppTypeReverse[bsonRecord.AppType],
				Amount:    bsonRecord.Amount,
			},
		}
		if len(bsonRecord.AppCosts) != 0 {
			costs := make(map[string]int64)
			for i := range bsonRecord.AppCosts {
				for j := range bsonRecord.AppCosts[i].UsedAmount {
					costs[resources.DefaultPropertyTypeLS.EnumMap[j].Name] += bsonRecord.AppCosts[i].UsedAmount[j]
				}
			}
			billingRecord.Costs = costs
		}
		if bsonRecord.Type == accountv1.Recharge {
			paymentAmount := billingRecord.Amount
			if bsonRecord.Payment != nil {
				paymentAmount = bsonRecord.Payment.Amount
			}
			billingRecord.Payment = &accountv1.PaymentForQuery{Amount: paymentAmount}
		}
		billingRecords = append(billingRecords, billingRecord)
	}

	totalCount := 0

	// total quantity
	cursorAll, err := billingColl.Aggregate(ctx, pipelineAll)
	if err != nil {
		return fmt.Errorf("failed to execute aggregate all query: %w", err)
	}
	defer cursorAll.Close(ctx)
	for cursorAll.Next(ctx) {
		var result struct {
			Result int64 `bson:"result"`
		}
		if err := cursorAll.Decode(&result); err != nil {
			return fmt.Errorf("failed to decode query count record: %w", err)
		}
		totalCount = int(result.Result)
	}

	// Costs Executing the second pipeline for getting the total count, recharge and deduction amount
	cursorCountAndAmount, err := billingColl.Aggregate(ctx, pipelineCountAndAmount)
	if err != nil {
		return fmt.Errorf("failed to execute aggregate query for count and amount: %w", err)
	}
	defer cursorCountAndAmount.Close(ctx)

	totalDeductionAmount := make(map[string]int64)
	totalRechargeAmount := int64(0)

	for cursorCountAndAmount.Next(ctx) {
		var result struct {
			ID struct {
				Type int    `bson:"type"`
				Key  string `bson:"key"`
			} `bson:"_id"`
			Total int64 `bson:"total"`
		}
		if err := cursorCountAndAmount.Decode(&result); err != nil {
			return fmt.Errorf("failed to decode billing record: %w", err)
		}
		if result.ID.Type == 0 {
			totalDeductionAmount[result.ID.Key] = result.Total
		}
	}

	// the total amount
	cursorRechargeAmount, err := billingColl.Aggregate(ctx, pipelineRechargeAmount)
	if err != nil {
		return fmt.Errorf("failed to execute aggregate query for recharge amount: %w", err)
	}
	defer cursorRechargeAmount.Close(ctx)

	for cursorRechargeAmount.Next(ctx) {
		var result struct {
			TotalRechargeAmount int64 `bson:"totalRechargeAmount"`
			Count               int   `bson:"count"`
		}
		if err := cursorRechargeAmount.Decode(&result); err != nil {
			return fmt.Errorf("failed to decode recharge amount record: %w", err)
		}
		totalRechargeAmount = result.TotalRechargeAmount
	}

	totalPages := (totalCount + billingRecordQuery.Spec.PageSize - 1) / billingRecordQuery.Spec.PageSize
	if totalCount == 0 {
		totalPages = 1
		totalCount = len(billingRecords)
	}
	billingRecordQuery.Status.Items, billingRecordQuery.Status.PageLength, billingRecordQuery.Status.TotalCount,
		billingRecordQuery.Status.RechargeAmount, billingRecordQuery.Status.DeductionAmount = billingRecords, totalPages, totalCount, totalRechargeAmount, totalDeductionAmount
	return nil
}

func (m *MongoDB) GetBillingCount(accountType accountv1.Type, startTime, endTime time.Time) (count, amount int64, err error) {
	filter := bson.M{
		"type": accountType,
		"time": bson.M{
			"$gte": startTime,
			"$lte": endTime,
		},
	}
	cursor, err := m.getBillingCollection().Find(context.Background(), filter)
	if err != nil {
		return 0, 0, err
	}
	defer cursor.Close(context.Background())
	var accountBalanceList []AccountBalanceSpecBSON
	err = cursor.All(context.Background(), &accountBalanceList)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to decode all billing record: %w", err)
	}
	for i := range accountBalanceList {
		count++
		amount += accountBalanceList[i].Amount
	}
	//for cursor.Next(context.Background()) {
	//    var accountBalance AccountBalanceSpecBSON
	//    if err := cursor.Decode(&accountBalance); err != nil {
	//        return 0, 0, err
	//    }
	//    count++
	//    amount += accountBalance.Amount
	//}
	return
}

func (m *MongoDB) getMeteringCollection() *mongo.Collection {
	return m.Client.Database(m.DBName).Collection(m.MeteringConn)
}

func (m *MongoDB) getMonitorCollection(collTime time.Time) *mongo.Collection {
	// 2020-12-01 00:00:00 - 2020-12-01 23:59:59
	return m.Client.Database(m.DBName).Collection(m.getMonitorCollectionName(collTime))
}

func (m *MongoDB) getMonitorCollectionName(collTime time.Time) string {
	// Calculate the suffix by day, for example, the suffix on the first day of 202012 is 20201201
	return fmt.Sprintf("%s_%s", m.MonitorConnPrefix, collTime.Format("20060102"))
}

func (m *MongoDB) getPricesCollection() *mongo.Collection {
	return m.Client.Database(m.DBName).Collection(m.PricesConn)
}

func (m *MongoDB) getBillingCollection() *mongo.Collection {
	return m.Client.Database(m.DBName).Collection(m.BillingConn)
}

func (m *MongoDB) getPropertiesCollection() *mongo.Collection {
	return m.Client.Database(m.DBName).Collection(m.PropertiesConn)
}

func (m *MongoDB) CreateBillingIfNotExist() error {
	if exist, err := m.collectionExist(m.DBName, m.BillingConn); exist || err != nil {
		return err
	}
	ctx := context.Background()
	err := m.Client.Database(m.DBName).CreateCollection(ctx, m.BillingConn)
	if err != nil {
		return fmt.Errorf("failed to create collection for billing: %w", err)
	}

	// create index
	_, err = m.getBillingCollection().Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			// unique index owner order_id
			Keys:    bson.D{primitive.E{Key: "owner", Value: 1}, primitive.E{Key: "order_id", Value: 1}},
			Options: options.Index().SetUnique(true),
		},
		{
			// owner + time + type indexes
			Keys: bson.D{
				primitive.E{Key: "owner", Value: 1},
				primitive.E{Key: "time", Value: 1},
				primitive.E{Key: "type", Value: 1},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create index for billing: %w", err)
	}
	return nil
}

// CreateMonitorTimeSeriesIfNotExist creates the time series table for monitor
func (m *MongoDB) CreateMonitorTimeSeriesIfNotExist(collTime time.Time) error {
	return m.CreateTimeSeriesIfNotExist(m.DBName, m.getMonitorCollectionName(collTime))
}

// CreateMeteringTimeSeriesIfNotExist creates the time series table for metering
func (m *MongoDB) CreateMeteringTimeSeriesIfNotExist() error {
	return m.CreateTimeSeriesIfNotExist(m.DBName, m.MeteringConn)
}

func (m *MongoDB) CreateTimeSeriesIfNotExist(dbName, collectionName string) error {
	// Check if the collection already exists
	if exist, err := m.collectionExist(dbName, collectionName); exist || err != nil {
		return err
	}

	// If the collection does not exist, create it
	cmd := bson.D{
		primitive.E{Key: "create", Value: collectionName},
		primitive.E{Key: "timeseries", Value: bson.D{{Key: "timeField", Value: "time"}}},
	}
	return m.Client.Database(dbName).RunCommand(context.TODO(), cmd).Err()
}

func (m *MongoDB) DropMonitorCollectionsOlderThan(days int) error {
	db := m.Client.Database(m.DBName)
	// Get the current time minus the number of days
	cutoffDate := time.Now().UTC().AddDate(0, 0, -days)
	cutoffName := m.getMonitorCollectionName(cutoffDate)

	collections, err := db.ListCollectionNames(context.Background(), bson.M{})
	if err != nil {
		return err
	}
	for i := range collections {
		// Check if the collection name starts with the prefix and is older than the cutoff date
		if strings.HasPrefix(collections[i], m.MonitorConnPrefix) && collections[i] < cutoffName {
			if err := db.Collection(collections[i]).Drop(context.TODO()); err != nil {
				return err
			}
			logger.Info("dropped collection: ", collections[i])
		}
	}
	return nil
}

func (m *MongoDB) collectionExist(dbName, collectionName string) (bool, error) {
	// Check if the collection already exists
	collections, err := m.Client.Database(dbName).ListCollectionNames(context.Background(), bson.M{"name": collectionName})
	return len(collections) > 0, err
}

func NewMongoDB(ctx context.Context, URL string) (Interface, error) {
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(URL))
	if err != nil {
		return nil, err
	}
	err = client.Ping(ctx, nil)
	return &MongoDB{
		Client:            client,
		URL:               URL,
		DBName:            DefaultDBName,
		MeteringConn:      DefaultMeteringConn,
		MonitorConnPrefix: DefaultMonitorConn,
		BillingConn:       DefaultBillingConn,
		PricesConn:        DefaultPricesConn,
		PropertiesConn:    DefaultPropertiesConn,
	}, err
}
