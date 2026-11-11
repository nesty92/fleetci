# gson: mvn verify and the javadoc jar, as its CI.
IMAGE=maven:3-eclipse-temurin-21
ENV=""
recipe() {
  step verify "mvn -B -ntp -Dmaven.repo.local=$C/m2 verify javadoc:jar"
}
