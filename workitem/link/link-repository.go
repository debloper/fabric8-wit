package link

import (
	"log"
	"strconv"

	"golang.org/x/net/context"

	"github.com/almighty/almighty-core/app"
	"github.com/almighty/almighty-core/errors"
	"github.com/almighty/almighty-core/gormsupport"
	"github.com/almighty/almighty-core/workitem"
	"github.com/jinzhu/gorm"
	satoriuuid "github.com/satori/go.uuid"
)

const (
	EndpointWorkItemTypes          = "workitemtypes"
	EndpointWorkItems              = "workitems"
	EndpointWorkItemLinkCategories = "workitemlinkcategories"
	EndpointWorkItemLinkTypes      = "workitemlinktypes"
	EndpointWorkItemLinks          = "workitemlinks"
)

// WorkItemLinkRepository encapsulates storage & retrieval of work item links
type WorkItemLinkRepository interface {
	Create(ctx context.Context, sourceID, targetID uint64, linkTypeID satoriuuid.UUID) (*app.WorkItemLinkSingle, error)
	Load(ctx context.Context, ID string) (*app.WorkItemLinkSingle, error)
	List(ctx context.Context) (*app.WorkItemLinkList, error)
	ListByWorkItemID(ctx context.Context, wiIDStr string) (*app.WorkItemLinkList, error)
	Delete(ctx context.Context, ID string) error
	Save(ctx context.Context, linkCat app.WorkItemLinkSingle) (*app.WorkItemLinkSingle, error)
}

// NewWorkItemLinkRepository creates a work item link repository based on gorm
func NewWorkItemLinkRepository(db *gorm.DB) *GormWorkItemLinkRepository {
	return &GormWorkItemLinkRepository{
		db:                   db,
		workItemRepo:         workitem.NewWorkItemRepository(db),
		workItemTypeRepo:     workitem.NewWorkItemTypeRepository(db),
		workItemLinkTypeRepo: NewWorkItemLinkTypeRepository(db),
	}
}

// GormWorkItemLinkRepository implements WorkItemLinkRepository using gorm
type GormWorkItemLinkRepository struct {
	db                   *gorm.DB
	workItemRepo         *workitem.GormWorkItemRepository
	workItemTypeRepo     *workitem.GormWorkItemTypeRepository
	workItemLinkTypeRepo *GormWorkItemLinkTypeRepository
}

// ValidateCorrectSourceAndTargetType returns an error if the Path of
// the source WIT as defined by the work item link type is not part of
// the actual source's WIT; the same applies for the target.
func (r *GormWorkItemLinkRepository) ValidateCorrectSourceAndTargetType(sourceID, targetID uint64, linkTypeID satoriuuid.UUID) error {
	linkType, err := r.workItemLinkTypeRepo.LoadTypeFromDBByID(linkTypeID)
	if err != nil {
		return err
	}
	// Fetch the source work item
	source, err := r.workItemRepo.LoadFromDB(strconv.FormatUint(sourceID, 10))
	if err != nil {
		return err
	}
	// Fetch the target work item
	target, err := r.workItemRepo.LoadFromDB(strconv.FormatUint(targetID, 10))
	if err != nil {
		return err
	}
	// Fetch the concrete work item types of the target and the source.
	sourceWorkItemType, err := r.workItemTypeRepo.LoadTypeFromDB(source.Type)
	if err != nil {
		return err
	}
	targetWorkItemType, err := r.workItemTypeRepo.LoadTypeFromDB(target.Type)
	if err != nil {
		return err
	}
	// Check type paths
	if !sourceWorkItemType.IsTypeOrSubtypeOf(linkType.SourceTypeName) {
		return errors.NewBadParameterError("source work item type", source.Type)
	}
	if !targetWorkItemType.IsTypeOrSubtypeOf(linkType.TargetTypeName) {
		return errors.NewBadParameterError("target work item type", target.Type)
	}
	return nil
}

// Create creates a new work item link in the repository.
// Returns BadParameterError, ConversionError or InternalError
func (r *GormWorkItemLinkRepository) Create(ctx context.Context, sourceID, targetID uint64, linkTypeID satoriuuid.UUID) (*app.WorkItemLinkSingle, error) {
	link := &WorkItemLink{
		SourceID:   sourceID,
		TargetID:   targetID,
		LinkTypeID: linkTypeID,
	}
	if err := link.CheckValidForCreation(); err != nil {
		return nil, err
	}
	if err := r.ValidateCorrectSourceAndTargetType(sourceID, targetID, linkTypeID); err != nil {
		return nil, err
	}
	db := r.db.Create(link)
	if db.Error != nil {
		if gormsupport.IsUniqueViolation(db.Error, "work_item_links_unique_idx") {
			// TODO(kwk): Make NewBadParameterError a variadic function to avoid this ugliness ;)
			return nil, errors.NewBadParameterError("data.relationships.source_id + data.relationships.target_id + data.relationships.link_type_id", sourceID).Expected("unique")
		}
		return nil, errors.NewInternalError(db.Error.Error())
	}
	// Convert the created link type entry into a JSONAPI response
	result := ConvertLinkFromModel(*link)
	return &result, nil
}

// Load returns the work item link for the given ID.
// Returns NotFoundError, ConversionError or InternalError
func (r *GormWorkItemLinkRepository) Load(ctx context.Context, ID string) (*app.WorkItemLinkSingle, error) {
	id, err := satoriuuid.FromString(ID)
	if err != nil {
		// treat as not found: clients don't know it must be a UUID
		return nil, errors.NewNotFoundError("work item link", ID)
	}
	log.Printf("loading work item link %s", id.String())
	res := WorkItemLink{}
	db := r.db.Where("id=?", id).Find(&res)
	if db.RecordNotFound() {
		log.Printf("not found work item link, res=%v", res)
		return nil, errors.NewNotFoundError("work item link", id.String())
	}
	if db.Error != nil {
		return nil, errors.NewInternalError(db.Error.Error())
	}
	// Convert the created link type entry into a JSONAPI response
	result := ConvertLinkFromModel(res)
	return &result, nil
}

type fetchLinksFunc func() ([]WorkItemLink, error)

func (r *GormWorkItemLinkRepository) list(ctx context.Context, fetchFunc fetchLinksFunc) (*app.WorkItemLinkList, error) {
	rows, err := fetchFunc()
	if err != nil {
		return nil, err
	}
	res := app.WorkItemLinkList{}
	res.Data = make([]*app.WorkItemLinkData, len(rows))
	for index, value := range rows {
		cat := ConvertLinkFromModel(value)
		res.Data[index] = cat.Data
	}
	// TODO: When adding pagination, this must not be len(rows) but
	// the overall total number of elements from all pages.
	res.Meta = &app.WorkItemLinkListMeta{
		TotalCount: len(rows),
	}
	return &res, nil
}

// ListByWorkItemID returns the work item links that have wiID as source or target.
// TODO: Handle pagination
func (r *GormWorkItemLinkRepository) ListByWorkItemID(ctx context.Context, wiIDStr string) (*app.WorkItemLinkList, error) {
	fetchFunc := func() ([]WorkItemLink, error) {
		var rows []WorkItemLink
		wi, err := r.workItemRepo.LoadFromDB(wiIDStr)
		if err != nil {
			return nil, err
		}
		// Now fetch all links for that work item
		db := r.db.Model(&WorkItemLink{}).Where("? IN (source_id, target_id)", wi.ID).Find(&rows)
		if db.Error != nil {
			return nil, db.Error
		}
		return rows, nil
	}
	return r.list(ctx, fetchFunc)
}

// List returns all work item links if wiID is nil; otherwise the work item links are returned
// that have wiID as source or target.
// TODO: Handle pagination
func (r *GormWorkItemLinkRepository) List(ctx context.Context) (*app.WorkItemLinkList, error) {
	fetchFunc := func() ([]WorkItemLink, error) {
		var rows []WorkItemLink
		db := r.db.Find(&rows)
		if db.Error != nil {
			return nil, db.Error
		}
		return rows, nil
	}
	return r.list(ctx, fetchFunc)
}

// Delete deletes the work item link with the given id
// returns NotFoundError or InternalError
func (r *GormWorkItemLinkRepository) Delete(ctx context.Context, ID string) error {
	id, err := satoriuuid.FromString(ID)
	if err != nil {
		// treat as not found: clients don't know it must be a UUID
		return errors.NewNotFoundError("work item link", ID)
	}
	var link = WorkItemLink{
		ID: id,
	}
	log.Printf("work item link to delete %v\n", link)
	db := r.db.Delete(&link)
	if db.Error != nil {
		log.Print(db.Error.Error())
		return errors.NewInternalError(db.Error.Error())
	}
	if db.RowsAffected == 0 {
		return errors.NewNotFoundError("work item link", id.String())
	}
	return nil
}

// Save updates the given work item link in storage. Version must be the same as the one int the stored version.
// returns NotFoundError, VersionConflictError, ConversionError or InternalError
func (r *GormWorkItemLinkRepository) Save(ctx context.Context, lt app.WorkItemLinkSingle) (*app.WorkItemLinkSingle, error) {
	res := WorkItemLink{}
	if lt.Data.ID == nil {
		return nil, errors.NewBadParameterError("work item link", nil)
	}
	db := r.db.Model(&res).Where("id=?", *lt.Data.ID).First(&res)
	if db.RecordNotFound() {
		log.Printf("work item link not found, res=%v", res)
		return nil, errors.NewNotFoundError("work item link", *lt.Data.ID)
	}
	if db.Error != nil {
		log.Print(db.Error.Error())
		return nil, errors.NewInternalError(db.Error.Error())
	}
	if lt.Data.Attributes.Version == nil || res.Version != *lt.Data.Attributes.Version {
		return nil, errors.NewVersionConflictError("version conflict")
	}
	if err := ConvertLinkToModel(lt, &res); err != nil {
		return nil, err
	}
	res.Version = res.Version + 1
	if err := r.ValidateCorrectSourceAndTargetType(res.SourceID, res.TargetID, res.LinkTypeID); err != nil {
		return nil, err
	}
	db = r.db.Save(&res)
	if db.Error != nil {
		log.Print(db.Error.Error())
		return nil, errors.NewInternalError(db.Error.Error())
	}
	log.Printf("updated work item link to %v\n", res)
	result := ConvertLinkFromModel(res)
	return &result, nil
}
