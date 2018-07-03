package mattermostapi

import (
	"log"

	"github.com/mattermost/mattermost-server/model"
)

type MatterMost struct {
	Url         string
	UserName    string
	Password    string
	UserId      string
	ChannelName string
	TeamName    string
	ChannelId   string
	TeamId      string
}

func (mm *MatterMost) GetClient() *model.Client {
	client := model.NewClient(mm.Url)
	r, e := client.Login(mm.UserName, mm.Password)
	if e != nil {
		log.Fatal("Couldn't login: ", e)
	}
	//log.Printf("Client logged in. Auth Token: %s.", client.AuthToken)
	user := r.Data.(*model.User)
	mm.UserId = user.Id
	//log.Println("User information: %s", user.ToJson())
	team, err := client.GetTeamByName(mm.TeamName)
	if err != nil {
		log.Fatal("Team Name not available")
	}
	mm.TeamId = team.Data.(*model.Team).Id
	//log.Println(mm.TeamId)
	client.SetTeamId(mm.TeamId)

	result, err := client.GetChannelByName(mm.ChannelName)
	if err != nil {
		log.Fatal("Channel Name not available")
	}
	mm.ChannelId = result.Data.(*model.Channel).Id
	//log.Println("Channle id ", mm.ChannelId)
	return client
}

func (mm *MatterMost) PostMessage(client *model.Client, messagetosend string) {

	newPost := model.Post{
		UserId:    mm.UserId,
		ChannelId: mm.ChannelId,
		Message:   messagetosend,
	}
	client.Login(mm.UserName, mm.Password)
	_, e := client.CreatePost(&newPost)
	if e != nil {
		log.Fatal("Couldn't make post: ", e)
	}
	//post := r.Data.(*model.Post)
	//log.Print("Post created: ", post)
}

func (mm *MatterMost) GetUserName(userid string, etag string) string {
	client := model.NewClient(mm.Url)
	r, _ := client.GetUser(userid, etag)
	return r.Data.(*model.User).FirstName
}
