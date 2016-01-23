package do

import (
	"github.com/bryanl/doit"
	"github.com/digitalocean/godo"
)

// Image is a werapper for godo.Image
type Image struct {
	*godo.Image
}

// Images is a slice of Droplet.
type Images []Image

// ImagesService is the godo ImagesService interface.
type ImagesService interface {
	List(public bool) (Images, error)
	ListDistribution(public bool) (Images, error)
	ListApplication(public bool) (Images, error)
	ListUser(public bool) (Images, error)
	GetByID(id int) (*Image, error)
	GetBySlug(slug string) (*Image, error)
	Update(id int, iur *godo.ImageUpdateRequest) (*Image, error)
	Delete(id int) error
}

type imagesService struct {
	client *godo.Client
}

var _ ImagesService = &imagesService{}

// NewImagesService builds an instance of ImagesService.
func NewImagesService(client *godo.Client) ImagesService {
	return &imagesService{
		client: client,
	}
}

func (is *imagesService) List(public bool) (Images, error) {
	return is.listImages(is.client.Images.List, public)
}

func (is *imagesService) ListDistribution(public bool) (Images, error) {
	return is.listImages(is.client.Images.ListDistribution, public)
}

func (is *imagesService) ListApplication(public bool) (Images, error) {
	return is.listImages(is.client.Images.ListApplication, public)
}

func (is *imagesService) ListUser(public bool) (Images, error) {
	return is.listImages(is.client.Images.ListUser, public)
}

func (is *imagesService) GetByID(id int) (*Image, error) {
	i, _, err := is.client.Images.GetByID(id)
	if err != nil {
		return nil, err
	}

	return &Image{Image: i}, nil
}

func (is *imagesService) GetBySlug(slug string) (*Image, error) {
	i, _, err := is.client.Images.GetBySlug(slug)
	if err != nil {
		return nil, err
	}

	return &Image{Image: i}, nil
}

func (is *imagesService) Update(id int, iur *godo.ImageUpdateRequest) (*Image, error) {
	i, _, err := is.client.Images.Update(id, iur)
	if err != nil {
		return nil, err
	}

	return &Image{Image: i}, nil
}

func (is *imagesService) Delete(id int) error {
	_, err := is.client.Images.Delete(id)
	return err
}

type listFn func(*godo.ListOptions) ([]godo.Image, *godo.Response, error)

func (is *imagesService) listImages(lFn listFn, public bool) (Images, error) {
	fn := func(opt *godo.ListOptions) ([]interface{}, *godo.Response, error) {
		list, resp, err := lFn(opt)
		if err != nil {
			return nil, nil, err
		}

		si := []interface{}{}
		for _, i := range list {
			if (public && i.Public) || !public {
				si = append(si, i)
			}
		}

		return si, resp, err
	}

	si, err := doit.PaginateResp(fn)
	if err != nil {
		return nil, err
	}

	var list Images
	for i := range si {
		image := si[i].(godo.Image)
		list = append(list, Image{Image: &image})
	}

	return list, nil
}
